// Package job runs long work off the request path.
//
// A short request starts the work and returns a job id at once. The work
// itself runs on a goroutine the Runner owns. Progress streams over the
// job's topic as server-sent events, and the terminal state is retrievable
// by id, so a page reload can catch up.
//
// A job outlives its request. The context the work receives keeps the
// request's values and drops its cancellation, so a client that goes away
// does not kill the work. Only Runner.Cancel stops a job.
//
// Jobs are durable behind a Store. The Runner records each job's state, its
// latest progress snapshot and its terminal result, so a restart reports
// what happened. On startup every unfinished job becomes interrupted, and a
// kind that declares itself idempotent may run again.
//
// The stream carries the event vocabulary of the wire package. A client
// subscribes to the job's topic first, then fetches the job's state, then
// deduplicates the terminal event on the job id. The fetched state carries a
// terminal a client missed, because the Store keeps it.
//
// Panic policy: a job's function is caller supplied and may panic. The
// Runner recovers at the job boundary, the one place a panic is a data
// point rather than a corrupted process, and converts it to an error
// terminal with ErrPanic in its chain. The process keeps serving.
package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nrynss/keel/id"
	"github.com/nrynss/keel/stream"
	"github.com/nrynss/keel/wire"
)

// TopicPrefix is the broker topic namespace every job publishes on. A job's
// topic is TopicPrefix + its id. The subscriber side hardcodes the
// convention, so it is pinned here and by TestTopicFormat.
const TopicPrefix = "job:"

// defaultKind is the kind Start runs when the caller names none.
const defaultKind = "default"

// defaultLimit is the number of jobs of one kind that run at once when the
// kind's Limit is unset. The bound keeps a bug or a burst from forking
// unbounded goroutines.
const defaultLimit = 16

// defaultMaxResultBytes is the largest result the Runner keeps inline when
// Config.MaxResultBytes is unset. A larger result belongs in a media store,
// with its id in the result, so the job row stays small.
const defaultMaxResultBytes = 1 << 20

// Unbounded disables the result cap when it is set as Config.MaxResultBytes.
const Unbounded = -1

// failureCode is the stable error code an error terminal carries in its
// wire envelope. A client branches on the code, so it never changes.
const failureCode = "job_failed"

// Status is a job's current or terminal state.
type Status string

// Status values. A terminal status is published as the event name of the
// terminal event and carried in Result.
const (
	// StatusRunning marks a job whose work is still on its goroutine.
	StatusRunning Status = "running"
	// StatusQueued marks a replacement attempt whose record is durable but
	// whose work has not started, because its kind is at its limit. It is
	// not terminal, and a restart starts it again rather than interrupting
	// it.
	StatusQueued Status = "queued"
	// StatusDone marks a job that finished on its own.
	StatusDone Status = "done"
	// StatusError marks a job that failed, panicked, or returned a result
	// over the configured cap.
	StatusError Status = "error"
	// StatusCancelled marks a job that ended because its context was
	// cancelled. Only Cancel holds a cancel handle, so this is the
	// cancelled terminal, not a failure.
	StatusCancelled Status = "cancelled"
	// StatusInterrupted marks a job that a restart ended before it
	// finished.
	StatusInterrupted Status = "interrupted"
)

// Sentinel errors. Every failure path this package produces wraps one of
// these, so callers branch with errors.Is.
var (
	// ErrUnknownJob is returned by Result, Cancel, or a Store for an id
	// that has no record.
	ErrUnknownJob = errors.New("job: unknown id")
	// ErrLimit is returned by Start when the job's kind is already at its
	// concurrency limit. Start never blocks for a slot, so saturation
	// surfaces here.
	ErrLimit = errors.New("job: kind at capacity")
	// ErrPanic wraps the value a job function panicked with. The terminal
	// status is StatusError like any other failure.
	ErrPanic = errors.New("job: panicked")
	// ErrNoBroker is returned by Open when no broker is configured.
	// Progress and terminal events are the product, so a Runner that
	// cannot publish them refuses to start.
	ErrNoBroker = errors.New("job: nil broker")
	// ErrInvalid is returned by Open for unusable configuration, such as a
	// nil store.
	ErrInvalid = errors.New("job: invalid config")
	// ErrResultTooLarge is returned to the work when its result is over
	// the configured cap. The terminal status is StatusError.
	ErrResultTooLarge = errors.New("job: result exceeds cap")
	// ErrInterrupted is carried by the terminal Result of a job a restart
	// ended.
	ErrInterrupted = errors.New("job: interrupted")
)

// Progress is one progress report a job publishes. The Runner adds the job
// id, so the report carries only what the work knows. Current and Total are
// optional, and Detail is optional raw JSON the application alone reads.
type Progress struct {
	// Stage names the step the work reached.
	Stage string `json:"stage,omitempty"`
	// Current is the work done so far, when the step counts.
	Current *int64 `json:"current,omitempty"`
	// Total is the work the step must do, when the step counts.
	Total *int64 `json:"total,omitempty"`
	// Detail is raw JSON the application attaches. The Runner passes it
	// through unchanged.
	Detail json.RawMessage `json:"detail,omitempty"`
}

// empty reports whether p carries nothing worth publishing. An empty
// report is dropped, because an eventless frame is wire noise.
func (p Progress) empty() bool {
	return p.Stage == "" && p.Current == nil && p.Total == nil && len(p.Detail) == 0
}

// Func is the work one job runs. It receives a context that is not cancelled
// when the starting request ends, because a job outlives its request. The
// context carries the request's values, and Runner.Cancel is the only handle
// that cancels it. A Func that returns promptly on ctx.Done() frees its slot
// and lands the cancelled terminal.
//
// progress reports a step to the job's topic. The Runner stamps the job id
// and publishes the wire progress payload. An empty report is dropped.
//
// Func owns its own deadlines. A long job must bound itself, because the
// Runner does not second-guess how long the work may take.
type Func func(ctx context.Context, progress func(Progress)) ([]byte, error)

// Result is one job's state at the time of a read. StatusQueued means the
// attempt has not started, StatusRunning means its work is on a goroutine,
// and the rest are terminal. Err is non-nil only for a failed, cancelled or
// interrupted job. Data carries the bytes the function returned, which a
// failed job keeps unless its result was over the cap.
type Result struct {
	// ID is the job id.
	ID string
	// Status is the job's state at the time of the read.
	Status Status
	// Data is the bytes the work returned. A failed job keeps them, and a result over the cap drops them.
	Data []byte
	// Err is the terminal error, nil for a done job.
	Err error
}

// Kind configures one class of job. Each kind runs under its own concurrency
// limit, so a heavy kind can run one at a time while a light kind runs
// several. The zero Kind runs defaultLimit jobs at once and is never
// resumed.
type Kind struct {
	// Limit is the number of jobs of this kind that run at once. Zero or
	// negative means defaultLimit.
	Limit int
	// Idempotent reports that running a job of this kind a second time is
	// safe, so the Runner may resume it after a restart. The zero value
	// refuses resumption, because a rerun of a paid call spends money
	// twice.
	Idempotent bool
	// MaxAttempts caps the attempts a job of this kind may make. Zero or
	// negative means one attempt, so nothing is resumed.
	MaxAttempts int
	// Resume rebuilds the work for a job of this kind that a restart left
	// unfinished. It receives the interrupted record, whose RootID names
	// the logical job. Nil means the kind cannot be resumed, even when it
	// is idempotent.
	Resume func(rec Record) (Func, error)
}

// Config configures Open. Broker and Store must both be set, because the
// Runner cannot publish without the first or remember without the second.
type Config struct {
	// Broker carries progress and terminal events. It must not be nil.
	Broker *stream.Broker
	// Store records job state, progress snapshots and results. It must not
	// be nil.
	Store Store
	// Kinds configures the named kinds. A kind absent from the map gets
	// the zero Kind, so it runs defaultLimit jobs at once and is never
	// resumed.
	Kinds map[string]Kind
	// Log receives one line per recorded fault, such as a store write that
	// failed. Nil means slog.Default.
	Log *slog.Logger
	// Now stamps each record's UpdatedAt. Nil means time.Now.
	Now func() time.Time
	// MaxResultBytes caps the inline result size. A larger result fails
	// the job with ErrResultTooLarge. Zero means defaultMaxResultBytes,
	// and Unbounded disables the cap.
	MaxResultBytes int
}

// Record is one job's storable state. A store keeps the current status, the
// latest progress snapshot and the terminal result.
//
// Attempt, ParentID and RootID record an attempt's lineage. The first
// attempt of a logical job has Attempt 1 and its own id as RootID. A rerun
// gets a fresh id, a ParentID naming the attempt it replaced, and the same
// RootID, so a restarted application can walk the chain with Store.Attempts.
type Record struct {
	// ID is the job id.
	ID string
	// Kind is the kind the job runs under.
	Kind string
	// Status is the job's current state.
	Status Status
	// Attempt counts the attempt within the logical job, starting at 1.
	Attempt int
	// ParentID is the id of the attempt this one replaced. Empty on a
	// first attempt.
	ParentID string
	// RootID is the id of the logical job's first attempt.
	RootID string
	// Progress is the latest progress snapshot. The zero value means none
	// was recorded.
	Progress Progress
	// Data is the terminal result bytes.
	Data []byte
	// Err is the terminal error. A durable store keeps its text, not its
	// chain, so a reader classifies on Status and reads Err for its
	// message.
	Err error
	// UpdatedAt is when the record last changed.
	UpdatedAt time.Time
}

// result maps a record to the catch-up Result.
func (rec Record) result() Result {
	return Result{ID: rec.ID, Status: rec.Status, Data: rec.Data, Err: rec.Err}
}

// Store records job state durably. This package declares the interface, and
// implementations live in their own package. An implementation must be safe
// for concurrent use, and must return an error matching ErrUnknownJob for an
// id it does not hold.
type Store interface {
	// Create stores a new record. It is called once, before the work
	// starts, so a racing Result finds the record.
	Create(ctx context.Context, rec Record) error
	// Begin marks a queued record as running. It updates Status and
	// UpdatedAt, called once when the attempt starts, so a process death
	// during the work leaves a running row. An id it does not hold
	// reports ErrUnknownJob.
	Begin(ctx context.Context, rec Record) error
	// SetProgress records the job's latest progress snapshot. It reads
	// ID, Progress and UpdatedAt.
	SetProgress(ctx context.Context, rec Record) error
	// Finish records the job's terminal state. It updates Status, Data, Err
	// and UpdatedAt, and keeps the Kind and lineage Create stored.
	Finish(ctx context.Context, rec Record) error
	// Get returns the record stored under id, or ErrUnknownJob.
	Get(ctx context.Context, id string) (Record, error)
	// Unfinished returns every record that has not reached a terminal
	// state, oldest first. Open interrupts a running record and queues a
	// queued one again.
	Unfinished(ctx context.Context) ([]Record, error)
	// Attempts returns every recorded attempt of the logical job that id
	// belongs to, oldest first. It lets a caller walk from an interrupted
	// attempt to the attempt that replaced it. An unknown id is
	// ErrUnknownJob.
	Attempts(ctx context.Context, id string) ([]Record, error)
}

// pendingJob is one replacement attempt whose record is already durable with
// StatusQueued and whose work has not started, because its kind is at its
// limit. The queued row survives a process death, so the next Open queues it
// again rather than interrupting it.
type pendingJob struct {
	rec Record
	fn  Func
}

// Runner starts jobs, publishes their progress and terminal events, and
// records their state behind a Store. One per process.
type Runner struct {
	broker *stream.Broker
	store  Store
	kinds  map[string]Kind
	log    *slog.Logger
	now    func() time.Time

	maxResultBytes int

	mu      sync.Mutex
	slots   map[string]chan struct{}
	cancels map[string]context.CancelFunc

	// pending holds each kind's durable replacement attempts that wait for a
	// slot, and draining marks a kind whose queue a drain goroutine already
	// owns. Both are guarded by mu.
	pending  map[string][]pendingJob
	draining map[string]bool
}

// Open returns a Runner that publishes to cfg.Broker and records through
// cfg.Store. It interrupts every job a previous process left running and
// publishes the interrupted terminal. It queues every queued attempt again
// and resumes the idempotent kinds that declare resume work and have attempts
// left.
func Open(ctx context.Context, cfg Config) (*Runner, error) {
	if cfg.Broker == nil {
		return nil, fmt.Errorf("job: open: %w", ErrNoBroker)
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("job: open: %w: nil store", ErrInvalid)
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	maxResultBytes := cfg.MaxResultBytes
	if maxResultBytes == 0 {
		maxResultBytes = defaultMaxResultBytes
	}
	r := &Runner{
		broker:         cfg.Broker,
		store:          cfg.Store,
		kinds:          cfg.Kinds,
		log:            log,
		now:            now,
		maxResultBytes: maxResultBytes,
		slots:          make(map[string]chan struct{}),
		cancels:        make(map[string]context.CancelFunc),
		pending:        make(map[string][]pendingJob),
		draining:       make(map[string]bool),
	}
	if err := r.recover(ctx); err != nil {
		return nil, fmt.Errorf("job: open: %w", err)
	}
	return r, nil
}

// Topic is the broker topic a job publishes on.
func Topic(id string) string {
	return TopicPrefix + id
}

// Start runs fn as a job of the default kind and returns the job id at once.
// See StartKind.
func (r *Runner) Start(ctx context.Context, fn Func) (string, error) {
	return r.StartKind(ctx, defaultKind, fn)
}

// StartKind runs fn as a job of the named kind and returns the job id at
// once. It never blocks for a slot, so a kind at its limit returns ErrLimit
// without starting anything.
//
// ctx is the starting request's context. Only its values are inherited, and
// its cancellation is dropped, because a client that goes away must not kill
// the work. The context fn receives is a Runner-owned child, and Runner.Cancel
// is the only handle that cancels it.
func (r *Runner) StartKind(ctx context.Context, kind string, fn Func) (string, error) {
	id, err := id.New()
	if err != nil {
		return "", fmt.Errorf("job: generate id: %w", err)
	}
	rec := Record{
		ID:        id,
		Kind:      kind,
		Status:    StatusRunning,
		Attempt:   1,
		RootID:    id,
		UpdatedAt: r.now(),
	}
	return r.launch(ctx, rec, fn)
}

// launch claims a slot, records rec, and runs fn on a Runner-owned
// goroutine. The id is the record's id. It never blocks for a slot, so a
// kind at its limit returns ErrLimit without starting anything.
func (r *Runner) launch(ctx context.Context, rec Record, fn Func) (string, error) {
	slot := r.slot(rec.Kind)
	select {
	case slot <- struct{}{}:
	default:
		return "", ErrLimit
	}
	return r.start(ctx, rec, fn, slot)
}

// start records rec and runs fn on a Runner-owned goroutine, holding the
// slot that launch already claimed. The job context keeps the request's
// values and drops its cancellation, so a client that goes away does not kill
// the work. The record is written with that same context, so a cancelled
// request cannot stop the record from landing.
func (r *Runner) start(ctx context.Context, rec Record, fn Func, slot chan struct{}) (string, error) {
	jobCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if err := r.store.Create(jobCtx, rec); err != nil {
		cancel()
		<-slot // release the slot: no job exists to hold it
		return "", fmt.Errorf("job: create record: %w", err)
	}
	r.spawn(jobCtx, cancel, rec, fn, slot)
	return rec.ID, nil
}

// spawn registers rec's cancel handle and starts the goroutine that runs it,
// holding the slot the caller already claimed.
func (r *Runner) spawn(jobCtx context.Context, cancel context.CancelFunc, rec Record, fn Func, slot chan struct{}) {
	r.mu.Lock()
	r.cancels[rec.ID] = cancel
	r.mu.Unlock()

	// The goroutine's exit path: run returns when fn returns or panics,
	// publishes the terminal event, records the terminal state, and releases
	// the slot and the cancel entry. Nothing else keeps it alive.
	go r.run(jobCtx, cancel, rec, fn, slot)
}

// slot returns the concurrency gate for kind, creating it on first use. A
// kind absent from Config gets the zero Kind, so its gate holds defaultLimit
// slots.
func (r *Runner) slot(kind string) chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch, ok := r.slots[kind]
	if !ok {
		limit := r.kinds[kind].Limit
		if limit <= 0 {
			limit = defaultLimit
		}
		ch = make(chan struct{}, limit)
		r.slots[kind] = ch
	}
	return ch
}

// Cancel asks a running job to stop: it cancels the Runner-owned context fn
// was handed. Cancellation is cooperative, because Go cannot kill a
// goroutine. A Func that returns promptly on ctx.Done() frees its slot, and
// one that ignores its context keeps going until it returns.
//
// When the error fn returned carries context.Canceled, the terminal is
// StatusCancelled instead of StatusError.
//
// Cancel is safe to call twice on a running job, and the second call is a
// no-op returning nil. An id this Runner never started, one that already
// terminated, or one from another process reports ErrUnknownJob.
func (r *Runner) Cancel(id string) error {
	r.mu.Lock()
	cancel, ok := r.cancels[id]
	r.mu.Unlock()
	if !ok {
		return ErrUnknownJob
	}
	cancel()
	return nil
}

// Result returns a job's state by id: StatusQueued for a replacement whose
// attempt has not started, StatusRunning while fn is on its goroutine, and
// the terminal Result afterwards. A client subscribes to Topic(id) first,
// then calls Result, then deduplicates the terminal event on the job id. A
// terminal the subscriber missed is found here, because the Store keeps it.
func (r *Runner) Result(ctx context.Context, id string) (Result, error) {
	rec, err := r.store.Get(ctx, id)
	if err != nil {
		return Result{}, err
	}
	return rec.result(), nil
}

// Attempts returns every recorded attempt of the logical job that id belongs
// to, oldest first. It lets a caller walk from an interrupted attempt to the
// attempt that replaced it.
func (r *Runner) Attempts(ctx context.Context, id string) ([]Record, error) {
	return r.store.Attempts(ctx, id)
}

// recover applies the restart policy. Every job a previous process left
// running becomes interrupted and publishes that terminal. A queued record
// never started, so it publishes no terminal and is queued for its attempt
// again. An idempotent kind with resume work and attempts left starts a fresh
// attempt, on its own id and topic.
func (r *Runner) recover(ctx context.Context) error {
	unfinished, err := r.store.Unfinished(ctx)
	if err != nil {
		return fmt.Errorf("list unfinished jobs: %w", err)
	}
	for _, rec := range unfinished {
		if rec.Status == StatusQueued {
			r.requeue(ctx, rec)
			continue
		}
		rec.Status = StatusInterrupted
		rec.Err = ErrInterrupted
		rec.UpdatedAt = r.now()
		if err := r.store.Finish(ctx, rec); err != nil {
			return fmt.Errorf("interrupt job %s: %w", rec.ID, err)
		}
		r.publishTerminal(rec.result())
		r.resume(ctx, rec)
	}
	return nil
}

// resume starts a fresh attempt of rec when its kind allows it. Only an
// idempotent kind with resume work and attempts left is rerun, because a
// rerun of a paid call spends money twice.
//
// The replacement record is written durably with StatusQueued before the
// attempt waits for a slot. A kind at its limit therefore leaves a queued
// record rather than dropping the attempt, so a process death before the
// attempt starts still resumes the chain. The attempt is charged only when it
// starts, so a queued record that never ran does not consume the limit. A
// record whose attempts are spent is never rerun, so a spent kind keeps its
// interrupted terminal. The replacement inherits the interrupted record's
// progress snapshot, so a resume hook can rebuild even a replacement that
// died before its first report.
func (r *Runner) resume(ctx context.Context, rec Record) {
	k := r.kinds[rec.Kind]
	if !k.Idempotent || k.Resume == nil {
		return
	}
	if rec.Attempt >= maxAttempts(k) {
		return
	}
	fn, err := k.Resume(rec)
	if err != nil {
		r.log.Error("job: resume work", "id", rec.ID, "kind", rec.Kind, "error", err)
		return
	}
	id, err := id.New()
	if err != nil {
		r.log.Error("job: resume id", "id", rec.ID, "kind", rec.Kind, "error", err)
		return
	}
	next := Record{
		ID:        id,
		Kind:      rec.Kind,
		Status:    StatusQueued,
		Attempt:   rec.Attempt + 1,
		ParentID:  rec.ID,
		RootID:    rec.RootID,
		Progress:  rec.Progress,
		UpdatedAt: r.now(),
	}
	if err := r.store.Create(context.WithoutCancel(ctx), next); err != nil {
		r.log.Error("job: resume record", "id", next.ID, "kind", next.Kind, "error", err)
		return
	}
	r.schedule(ctx, next, fn)
}

// requeue starts a durable queued attempt that a restart left behind. The
// attempt never started, so it publishes no terminal and keeps its attempt
// number. Only an idempotent kind with resume work and an attempt within the
// limit is queued again.
func (r *Runner) requeue(ctx context.Context, rec Record) {
	k := r.kinds[rec.Kind]
	if !k.Idempotent || k.Resume == nil {
		return
	}
	if rec.Attempt > maxAttempts(k) {
		return
	}
	fn, err := k.Resume(rec)
	if err != nil {
		r.log.Error("job: resume work", "id", rec.ID, "kind", rec.Kind, "error", err)
		return
	}
	r.schedule(ctx, rec, fn)
}

// maxAttempts returns the attempt cap for k. A non-positive cap reads as one,
// so a kind that declares no cap never resumes.
func maxAttempts(k Kind) int {
	if k.MaxAttempts < 1 {
		return 1
	}
	return k.MaxAttempts
}

// schedule queues the durable replacement attempt rec for execution under
// its kind's limit. It never blocks, so a saturated kind cannot stall the
// startup recovery that calls it. One drain goroutine per kind starts queued
// attempts as slots free, so the live goroutine count does not grow with the
// number of queued records.
func (r *Runner) schedule(ctx context.Context, rec Record, fn Func) {
	r.mu.Lock()
	r.pending[rec.Kind] = append(r.pending[rec.Kind], pendingJob{rec: rec, fn: fn})
	if r.draining[rec.Kind] {
		r.mu.Unlock()
		return
	}
	r.draining[rec.Kind] = true
	r.mu.Unlock()

	// The goroutine's exit path: drain returns once this kind's queue is
	// empty, and at most one drain goroutine runs per kind at a time.
	go r.drain(ctx, rec.Kind)
}

// drain starts the queued attempts of one kind as slots free, oldest first.
// It returns once the kind's queue is empty, so at most one drain goroutine
// runs per kind and the count does not grow with the number of records.
func (r *Runner) drain(ctx context.Context, kind string) {
	slot := r.slot(kind)
	for {
		r.mu.Lock()
		queue := r.pending[kind]
		if len(queue) == 0 {
			r.draining[kind] = false
			r.mu.Unlock()
			return
		}
		item := queue[0]
		queue[0] = pendingJob{} // release the closure the popped slot held
		r.pending[kind] = queue[1:]
		r.mu.Unlock()

		slot <- struct{}{} // block until the kind has capacity
		item.rec.Status = StatusRunning
		item.rec.UpdatedAt = r.now()
		if err := r.store.Begin(context.WithoutCancel(ctx), item.rec); err != nil {
			r.log.Error("job: begin attempt", "id", item.rec.ID, "kind", item.rec.Kind, "error", err)
		}
		jobCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		r.spawn(jobCtx, cancel, item.rec, item.fn, slot)
	}
}

// run executes one job. It is the only place terminal state is recorded and
// the terminal event is published, once, on every path.
func (r *Runner) run(ctx context.Context, cancel context.CancelFunc, rec Record, fn Func, slot chan struct{}) {
	defer func() {
		r.mu.Lock()
		delete(r.cancels, rec.ID)
		r.mu.Unlock()
		cancel() // release the job context's resources
		<-slot
	}()
	// The job context cancels on Cancel, so the record writes use a context
	// detached from it. The terminal and the last progress snapshot must land
	// even when the function returns because its context was cancelled.
	writeCtx := context.WithoutCancel(ctx)

	progress := func(p Progress) {
		if p.empty() {
			return
		}
		r.publishProgress(rec.ID, p)
		snap := rec
		snap.Progress = p
		snap.UpdatedAt = r.now()
		if err := r.store.SetProgress(writeCtx, snap); err != nil {
			r.log.Warn("job: record progress", "id", rec.ID, "kind", rec.Kind, "error", err)
		}
	}

	data, err := call(ctx, fn, progress)

	res := Result{ID: rec.ID, Data: data}
	switch {
	case err == nil && r.overCap(len(data)):
		res.Status = StatusError
		res.Err = fmt.Errorf("%w: %d bytes over %d", ErrResultTooLarge, len(data), r.maxResultBytes)
		res.Data = nil
	case err == nil:
		res.Status = StatusDone
	case errors.Is(err, context.Canceled):
		res.Status = StatusCancelled
		res.Err = err
	default:
		res.Status = StatusError
		res.Err = err
	}

	terminal := Record{
		ID:        rec.ID,
		Status:    res.Status,
		Data:      res.Data,
		Err:       res.Err,
		UpdatedAt: r.now(),
	}
	if err := r.store.Finish(writeCtx, terminal); err != nil {
		r.log.Error("job: record terminal", "id", rec.ID, "kind", rec.Kind, "error", err)
	}
	r.publishTerminal(res)
}

// overCap reports whether a result of n bytes is over the configured cap.
func (r *Runner) overCap(n int) bool {
	return r.maxResultBytes != Unbounded && n > r.maxResultBytes
}

// publishProgress publishes one progress event on the job's topic.
func (r *Runner) publishProgress(id string, p Progress) {
	event := wire.ProgressEvent{
		JobID:   id,
		Stage:   p.Stage,
		Current: p.Current,
		Total:   p.Total,
		Detail:  p.Detail,
	}
	r.broker.Publish(Topic(id), stream.Event{Name: wire.EventProgress, Data: event})
}

// publishTerminal publishes the one terminal event for a job. A failure
// carries the wire error envelope, and every other status carries the wire
// status payload. The event is marked terminal, so the broker retains it for
// a subscriber that joins after the job ended.
func (r *Runner) publishTerminal(res Result) {
	name := string(res.Status)
	var payload any
	if res.Status == StatusError {
		event, err := wire.NewErrorEvent(res.ID, failureCode, errorText(res.Err), nil)
		if err != nil {
			r.log.Error("job: encode error terminal", "id", res.ID, "error", err)
			event = wire.ErrorEvent{JobID: res.ID, Status: name}
		}
		payload = event
	} else {
		payload = wire.StatusEvent{JobID: res.ID, Status: name}
	}
	r.broker.Publish(Topic(res.ID), stream.Event{Name: name, Data: payload, Terminal: true})
}

// errorText returns an error's message, or the empty string for a nil error.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// call invokes fn with the panic boundary around it: a panic becomes an
// ErrPanic-wrapped error and a nil result, and the deferred recover is the
// only reason the process survives the panicking goroutine.
func call(ctx context.Context, fn Func, progress func(Progress)) (data []byte, err error) {
	defer func() {
		if p := recover(); p != nil {
			data = nil
			err = fmt.Errorf("%w: %v", ErrPanic, p)
		}
	}()
	return fn(ctx, progress)
}
