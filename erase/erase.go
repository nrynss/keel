// Package erase fans a delete out over named targets until every one
// confirms.
//
// A delete that touches rows, blobs and a remote provider is not done when
// the first target answers. It is done when every target confirms, and it
// must survive a restart in between. Deleting is safe to repeat, so the
// erasure repeats every delete that has not confirmed. A target that
// reports already gone with ErrGone counts as confirmed too.
//
// The consumer registers the targets. The library owns the fan-out, the
// retry and the record of which targets confirmed. It never owns the list,
// so a Source rebuilds the list from the ref when a run resumes.
//
// The work runs as a job kind, so cancellation, per kind limits and
// resumption after a restart come from the job package. Every confirmation
// lands in the job record, so a resumed run retries only what has not
// confirmed, and Inspect reads the ledger back at any time. A run that ends
// with targets that never confirmed is a stuck erasure: the job fails, and
// Report names every target still owed a delete. Nothing turns a stuck
// erasure into a success.
package erase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nrynss/keel/job"
)

// KindName is the job kind every erasure runs under. Register the value the
// Kind method returns under this name in job.Config.Kinds before the Runner
// opens, because a kind that declares no resume work is never resumed.
const KindName = "erase"

// defaultTargetAttempts is the tries one target gets within one run when
// Config.TargetAttempts is unset.
const defaultTargetAttempts = 3

// defaultFanOut is the number of targets deleted at once when
// Config.FanOut is unset. The bound keeps a long target list from forking
// unbounded goroutines.
const defaultFanOut = 8

// defaultMaxAttempts is the attempts one erasure may make across restarts
// when Config.MaxAttempts is unset. Every restart may resume the work,
// because deleting is safe to repeat.
const defaultMaxAttempts = 5

// Sentinel errors. Every failure path this package produces wraps one of
// these, so callers branch with errors.Is.
var (
	// ErrGone is returned by a Target whose object is already absent. An
	// erasure counts that answer as a confirmation, because deleting is
	// safe to repeat. Wrap it with %w so errors.Is finds it.
	ErrGone = errors.New("erase: target already gone")

	// ErrInvalid is returned for unusable input, such as a nil source, a
	// nil runner, or a target list with an empty or repeated name.
	ErrInvalid = errors.New("erase: invalid input")

	// ErrStuck is carried by the error a run returns when targets spent
	// their tries without confirming. The message names every target the
	// erasure is still owed.
	ErrStuck = errors.New("erase: stuck targets")

	// ErrNoSnapshot is returned by Inspect for an erasure whose records
	// carry no progress snapshot, because every run ended before its
	// first report landed.
	ErrNoSnapshot = errors.New("erase: no recorded snapshot")
)

// Target is one named delete operation the consumer supplies. The library
// decides when Delete runs, and never owns the list of targets.
type Target interface {
	// Name identifies the target in progress reports and in a stuck
	// report. It must be non-empty and unique within one erasure.
	Name() string

	// Delete removes the target's object. A nil return confirms the
	// delete, and an error carrying ErrGone confirms it too, because the
	// object is already absent. Delete must be safe to call again,
	// because a run that resumes repeats every delete that has not
	// confirmed.
	Delete(ctx context.Context) error
}

// Source rebuilds the target list of one erasure from the ref Start
// received. The consumer owns the list, so only the consumer can rebuild it
// for a run that resumes after a restart. A Source returns the same list
// for the same ref, because a target the list drops while it is still
// unconfirmed fails the erasure as stuck.
type Source func(ctx context.Context, ref string) ([]Target, error)

// Config configures New.
type Config struct {
	// TargetAttempts is the tries one target gets within one run before
	// the erasure names it stuck. Zero or negative means
	// defaultTargetAttempts.
	TargetAttempts int

	// RetryDelay separates two tries of the same target. Zero retries at
	// once.
	RetryDelay time.Duration

	// FanOut is the number of targets deleted at once. Zero or negative
	// means defaultFanOut.
	FanOut int

	// MaxAttempts is the number of attempts one erasure may make across
	// restarts. Zero or negative means defaultMaxAttempts.
	MaxAttempts int

	// Log receives one line per failed try. Nil means slog.Default.
	Log *slog.Logger
}

// Eraser runs erasures as jobs of one kind on a job Runner. Create it with
// New. Eraser is safe for concurrent use, because every field is read-only
// after New returns.
type Eraser struct {
	source      Source
	attempts    int
	retryDelay  time.Duration
	fanOut      int
	maxAttempts int
	log         *slog.Logger
}

// New returns an Eraser that runs erasures over the target lists source
// rebuilds. Source must not be nil, because a run that resumes can only
// delete what the consumer still names.
func New(source Source, cfg Config) (*Eraser, error) {
	if source == nil {
		return nil, fmt.Errorf("erase: new: %w: nil source", ErrInvalid)
	}
	e := &Eraser{
		source:      source,
		attempts:    cfg.TargetAttempts,
		retryDelay:  cfg.RetryDelay,
		fanOut:      cfg.FanOut,
		maxAttempts: cfg.MaxAttempts,
		log:         cfg.Log,
	}
	if e.attempts < 1 {
		e.attempts = defaultTargetAttempts
	}
	if e.fanOut < 1 {
		e.fanOut = defaultFanOut
	}
	if e.maxAttempts < 1 {
		e.maxAttempts = defaultMaxAttempts
	}
	if e.log == nil {
		e.log = slog.Default()
	}
	return e, nil
}

// Kind returns the job kind that runs erasures. Register it under KindName
// in job.Config.Kinds before opening the Runner, because a kind that
// declares no resume work is never resumed. The kind is idempotent, because
// deleting is safe to repeat. Its Limit stays at the job package's default,
// so a consumer that wants another bound edits Limit on the returned value
// before registering it.
func (e *Eraser) Kind() job.Kind {
	return job.Kind{
		Idempotent:  true,
		MaxAttempts: e.maxAttempts,
		Resume:      e.resume,
	}
}

// Start runs one erasure over targets as a job of KindName on r and returns
// the job id at once. ref is the application key Source receives when a
// restart rebuilds the list. Every name must be non-empty and unique, and
// the list must not be empty, because an erasure over nothing is never what
// a caller means. r must be the Runner the kind returned by Kind is
// registered under, because a restart rebuilds the work through it. A
// Runner opened without the kind still runs the erasure, but a restart
// leaves it interrupted.
func (e *Eraser) Start(ctx context.Context, r *job.Runner, ref string, targets []Target) (string, error) {
	if r == nil {
		return "", fmt.Errorf("erase: start: %w: nil runner", ErrInvalid)
	}
	if len(targets) == 0 {
		return "", fmt.Errorf("erase: start: %w: no targets", ErrInvalid)
	}
	if err := validate(targets); err != nil {
		return "", fmt.Errorf("erase: start: %w", err)
	}
	fn := func(ctx context.Context, progress func(job.Progress)) ([]byte, error) {
		return e.run(ctx, progress, ref, targets, snapshot{})
	}
	id, err := r.StartKind(ctx, KindName, fn)
	if err != nil {
		return "", fmt.Errorf("erase: start: %w", err)
	}
	return id, nil
}

// Report is one erasure's ledger as Inspect read it from the job records.
type Report struct {
	// Ref is the application key Start received.
	Ref string

	// Confirmed lists every target that confirmed, in name order.
	Confirmed []string

	// Stuck lists every registered target with no confirmation, in name
	// order. Empty means the erasure is complete.
	Stuck []string
}

// Complete reports whether every registered target confirmed. Only a
// complete erasure may be reported as erased.
func (rep Report) Complete() bool {
	return len(rep.Stuck) == 0
}

// Inspect reads one erasure's ledger from r's records. The id is the job id
// Start returned, or any attempt id of the same logical job, because the
// last attempt that reported carries the fullest ledger. An erasure whose
// records carry no snapshot reports ErrNoSnapshot.
func (e *Eraser) Inspect(ctx context.Context, r *job.Runner, id string) (Report, error) {
	attempts, err := r.Attempts(ctx, id)
	if err != nil {
		return Report{}, fmt.Errorf("erase: inspect %s: %w", id, err)
	}
	if len(attempts) == 0 {
		return Report{}, fmt.Errorf("erase: inspect %s: no attempts recorded", id)
	}
	if attempts[len(attempts)-1].Kind != KindName {
		return Report{}, fmt.Errorf("erase: inspect %s: %w: job is a %s job",
			id, ErrInvalid, attempts[len(attempts)-1].Kind)
	}
	var s snapshot
	found := false
	for i := len(attempts) - 1; i >= 0 && !found; i-- {
		s, err = decodeSnapshot(attempts[i].Progress.Detail)
		switch {
		case err == nil:
			found = true
		case errors.Is(err, ErrNoSnapshot):
			// The attempt ended before its first report. An older
			// attempt may still hold the ledger it handed over.
		default:
			return Report{}, fmt.Errorf("erase: inspect %s: %w", id, err)
		}
	}
	if !found {
		return Report{}, fmt.Errorf("erase: inspect %s: %w", id, ErrNoSnapshot)
	}
	confirmed := append([]string(nil), s.Confirmed...)
	sort.Strings(confirmed)
	return Report{Ref: s.Ref, Confirmed: confirmed, Stuck: missing(s.Targets, confirmed)}, nil
}

// resume rebuilds the work of an erasure a restart left unfinished. It is
// the job kind's Resume hook. A record whose snapshot is missing or unread
// cannot be rebuilt, because no run can name what nothing recorded.
func (e *Eraser) resume(rec job.Record) (job.Func, error) {
	s, err := decodeSnapshot(rec.Progress.Detail)
	if err != nil {
		return nil, fmt.Errorf("erase: resume %s: %w", rec.ID, err)
	}
	return func(ctx context.Context, progress func(job.Progress)) ([]byte, error) {
		targets, err := e.source(ctx, s.Ref)
		if err != nil {
			return nil, fmt.Errorf("erase: source %s: %w", s.Ref, err)
		}
		if err := validate(targets); err != nil {
			return nil, fmt.Errorf("erase: resume %s: %w", rec.ID, err)
		}
		return e.run(ctx, progress, s.Ref, targets, s)
	}, nil
}

// run drives one attempt of the erasure job. prior carries the ledger the
// attempt inherited from an interrupted record, and a fresh run passes the
// zero value.
func (e *Eraser) run(ctx context.Context, progress func(job.Progress), ref string, targets []Target, prior snapshot) ([]byte, error) {
	names := unionNames(prior.Targets, targetNames(targets))
	confirmed := make(map[string]bool, len(names))
	for _, name := range prior.Confirmed {
		confirmed[name] = true
	}
	publish := func() {
		e.publish(progress, buildSnapshot(ref, names, confirmed))
	}
	// The lock spans the ledger update and the report write, so a later
	// ledger can never be overwritten by an earlier one.
	var mu sync.Mutex
	confirm := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		confirmed[name] = true
		publish()
	}

	// The pending pass reads the ledger before any goroutine starts, so
	// it needs no lock.
	var pending []Target
	for _, t := range targets {
		if !confirmed[t.Name()] {
			pending = append(pending, t)
		}
	}
	publish()

	// The fan-out holds at most e.fanOut deletes at once, and the send
	// into sem blocks the loop when the bound is reached. Every
	// goroutine's exit path is the return of its closure, after the
	// target confirmed or spent its tries. A spent target records
	// nothing, because the ledger decides the stuck set at the end.
	var wg sync.WaitGroup
	sem := make(chan struct{}, e.fanOut)
	for _, t := range pending {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := e.eraseOne(ctx, t); err == nil {
				confirm(t.Name())
			}
		}()
	}
	wg.Wait()

	// A cancellation ends the run before stuck targets do, because the
	// job package reports it as cancelled rather than as a failure.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("erase: %w", err)
	}
	// The ledger decides the stuck set, so a target that the rebuilt
	// list dropped is owed a delete here just like one whose tries ran
	// out.
	if owed := missing(names, confirmedNames(confirmed)); len(owed) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrStuck, strings.Join(owed, ", "))
	}
	return nil, nil
}

// eraseOne deletes one target until it confirms or spends its tries. A nil
// return means confirmed, whether the delete happened or the target was
// already gone.
func (e *Eraser) eraseOne(ctx context.Context, t Target) error {
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("erase: target %s: %w", t.Name(), err)
		}
		err := t.Delete(ctx)
		if err == nil || errors.Is(err, ErrGone) {
			return nil
		}
		e.log.Warn("erase: delete failed", "target", t.Name(), "attempt", attempt, "error", err)
		if attempt >= e.attempts {
			return fmt.Errorf("erase: target %s: %w", t.Name(), err)
		}
		if e.retryDelay > 0 && !e.pause(ctx, e.retryDelay) {
			return fmt.Errorf("erase: target %s: %w", t.Name(), ctx.Err())
		}
	}
}

// pause waits d or until ctx is done. It reports whether the wait
// completed.
func (e *Eraser) pause(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// publish reports one ledger snapshot through the job's progress channel,
// so the Runner both streams it and leaves it durable in the record.
func (e *Eraser) publish(progress func(job.Progress), s snapshot) {
	detail, err := json.Marshal(s)
	if err != nil {
		// The snapshot holds only strings, so Marshal cannot fail. A
		// snapshot that cannot encode is dropped, and the next one
		// carries the full ledger again.
		e.log.Error("erase: encode ledger", "ref", s.Ref, "error", err)
		return
	}
	current := int64(len(s.Confirmed))
	total := int64(len(s.Targets))
	progress(job.Progress{Current: &current, Total: &total, Detail: detail})
}

// snapshot is the ledger one progress report carries in the job record's
// Detail. A run that resumes reads it from the interrupted record, so only
// unconfirmed targets delete again.
type snapshot struct {
	Ref       string   `json:"ref,omitempty"`
	Targets   []string `json:"targets,omitempty"`
	Confirmed []string `json:"confirmed,omitempty"`
}

// decodeSnapshot reads a ledger from a progress Detail. An empty Detail is
// ErrNoSnapshot, because the run ended before its first report.
func decodeSnapshot(detail json.RawMessage) (snapshot, error) {
	if len(detail) == 0 {
		return snapshot{}, ErrNoSnapshot
	}
	var s snapshot
	if err := json.Unmarshal(detail, &s); err != nil {
		return snapshot{}, fmt.Errorf("decode ledger: %w", err)
	}
	return s, nil
}

// buildSnapshot renders the ledger into the shape one progress report
// carries. Both lists come out in name order, because names drives the
// ledger.
func buildSnapshot(ref string, names []string, confirmed map[string]bool) snapshot {
	s := snapshot{Ref: ref, Targets: names}
	for _, name := range names {
		if confirmed[name] {
			s.Confirmed = append(s.Confirmed, name)
		}
	}
	return s
}

// validate refuses a target list a run cannot ledger: a nil target, an
// empty name, or a name registered twice.
func validate(targets []Target) error {
	seen := make(map[string]bool, len(targets))
	for i, t := range targets {
		if t == nil {
			return fmt.Errorf("%w: target %d is nil", ErrInvalid, i)
		}
		name := t.Name()
		if name == "" {
			return fmt.Errorf("%w: target %d has an empty name", ErrInvalid, i)
		}
		if seen[name] {
			return fmt.Errorf("%w: target %s is registered twice", ErrInvalid, name)
		}
		seen[name] = true
	}
	return nil
}

// targetNames returns each target's name, in registration order.
func targetNames(targets []Target) []string {
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, t.Name())
	}
	return names
}

// unionNames merges the name lists into one sorted list without repeats.
func unionNames(lists ...[]string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, list := range lists {
		for _, name := range list {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// missing returns every name in all that done does not hold, in name order.
func missing(all, done []string) []string {
	set := make(map[string]bool, len(done))
	for _, name := range done {
		set[name] = true
	}
	var out []string
	for _, name := range all {
		if !set[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// confirmedNames returns the map's keys in name order.
func confirmedNames(confirmed map[string]bool) []string {
	out := make([]string, 0, len(confirmed))
	for name := range confirmed {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
