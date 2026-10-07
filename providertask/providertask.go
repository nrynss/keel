// Package providertask runs provider work that is created and then polled.
//
// Many paid APIs work one way. A create call returns a task id. A status
// call reports running, succeeded or failed. A result call returns the
// outcome once the task succeeded. The task id stays queryable for a
// limited window, and the result link it carries expires hours later.
//
// Run carries one task from create to result under a caller chosen
// idempotency key. The key is the promise. Run records the task id in a
// Store before it polls, so a restart that calls Run again with the same
// key resumes the recorded task and never calls Create again. Nothing
// re-creates a recorded task silently, because a second create is a second
// charge.
//
// A recorded task that still runs when its age passes Config.Window
// refuses with ErrWindowExceeded and its stable code. The provider may no
// longer answer queries about the task, so polling it would spin. The
// refusal names the fact instead of papering over it, and the caller
// resolves the key out of band. A recorded verdict is honoured at any age,
// because the provider already answered it.
//
// A key that is claimed but carries no task id yet is a create some Run
// started and has not recorded. Another Run waits for that record rather
// than duplicating it. A Run that finds no outcome inside its deadline
// refuses with ErrTaskPending, whether the key still carries no task id or
// a recorded task has not finished. A create that failed mid call leaves
// the key in that state on purpose, because the provider may have created
// the task even though the answer was lost. Every later Run of a burned
// key waits out its own deadline before refusing, thirty minutes by
// default, and Store.Release removes a still empty claim once nothing is
// believed to have landed, which is the deliberate recovery.
//
// Polling waits on the upper half jitter the throttle package uses, from
// Config.PollBase to Config.PollCeiling, bounded by Config.Deadline. Each
// wait spans half the current step to the whole step, so the first wait
// lands between half PollBase and PollBase. A Status error the configured
// Retryable classifies as transient is retried on that curve. A failed
// verdict from the provider is a terminal answer, never a retry.
//
// Run composes with a cost meter. The Run that creates the task reserves
// Config.Estimate before it creates, settles when the task succeeds, and
// frees the reservation when it fails. A provider that charges for
// failures settles the estimate on failure instead. The settle and the
// terminal record are two writes. A crash between them books the charge
// again on the next resume, so the books over-count spend rather than
// under-count it. The provider billed once either way.
//
// The result link expires, so Run hands the result to Spec.Keep the
// moment it has it, before Run returns. Keep may run once per Run that
// reaches the result, so it must tolerate repeating. Copying under a name
// derived from the idempotency key makes a repeat land on the same bytes.
//
// Run publishes progress into a job, and a job carries it to the browser
// over the stream. Run publishes the created stage once the task id is
// recorded, then passes the provider progress through.
//
// Provider webhooks are out of scope. A later Spec field could accept a
// webhook signal in place of the polling loop without changing the store
// or the resume promise.
package providertask

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"sync"
	"time"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/job"
	"github.com/nrynss/keel/throttle"
)

// The wire codes a Run refuses with. Every code is a stable snake_case
// identifier a handler passes to the error envelope unchanged.
const (
	// CodeWindowExceeded names a task id whose recorded age passed the
	// provider's query window. The key is never re-created.
	CodeWindowExceeded = "task_window_exceeded"
	// CodeTaskPending names a key whose outcome is not resolved yet: a
	// claimed key that still carries no task id, or a recorded task that
	// did not finish inside the caller's deadline. The claim may still
	// land, so the caller asks again rather than re-runs.
	CodeTaskPending = "task_pending"
	// CodeDeadline names a task that did not reach a terminal state inside
	// Config.Deadline. The task keeps running at the provider.
	CodeDeadline = "task_timeout"
)

// State names where one task stands at the provider.
type State string

// Task states. A task moves from running to succeeded or failed, and a
// terminal state never changes again.
const (
	// StateRunning marks a task that is created and not terminal yet.
	StateRunning State = "running"
	// StateSucceeded marks a task the provider finished successfully.
	StateSucceeded State = "succeeded"
	// StateFailed marks a task the provider finished with an error.
	StateFailed State = "failed"
)

// StageCreated is the progress stage Run publishes once the provider task
// id is recorded and polling is about to start.
const StageCreated = "created"

// Sentinel errors. Every refusal this package produces wraps one of these,
// so callers branch with errors.Is and handlers map the Code constants.
var (
	// ErrInvalid is returned by Run for unusable input, such as a nil
	// store, an empty key, or a Spec missing a required function.
	ErrInvalid = errors.New("providertask: invalid input")
	// ErrUnknownKey is returned by a Store for a key it holds no row for.
	ErrUnknownKey = errors.New("providertask: unknown key")
	// ErrWindowExceeded reports a recorded task that still runs when its
	// age passed the provider's query window. The refusal never re-creates
	// the task, and a recorded verdict is never refused.
	ErrWindowExceeded = errors.New("providertask: task id is older than the query window")
	// ErrTaskPending reports a key whose create is still unresolved, or
	// whose task has not finished, when the deadline passed. The claim may
	// still land, so the caller asks again rather than re-runs.
	ErrTaskPending = errors.New("providertask: task is still unresolved")
	// ErrDeadline reports a task that did not reach a terminal state
	// inside Config.Deadline. The task keeps running at the provider.
	ErrDeadline = errors.New("providertask: deadline passed before the task finished")
	// ErrTaskFailed is wrapped by TaskFailure, the provider's terminal
	// verdict that the task failed.
	ErrTaskFailed = errors.New("providertask: task failed")
)

// Defaults for Config. The zero value of every duration field reads as its
// default, so a plain Config polls a task for half an hour at provider
// friendly gaps.
const (
	// DefaultWindow is how long a recorded task id is trusted to stay
	// queryable when Config.Window is unset.
	DefaultWindow = 24 * time.Hour
	// DefaultDeadline bounds one Run when Config.Deadline is unset.
	DefaultDeadline = 30 * time.Minute
	// DefaultPollBase is the wait before the second status poll when
	// Config.PollBase is unset.
	DefaultPollBase = 2 * time.Second
	// DefaultPollCeiling caps one wait between status polls when
	// Config.PollCeiling is unset.
	DefaultPollCeiling = 30 * time.Second
)

// defaultKind names the billed operation on ledger rows when Config.Kind
// is empty.
const defaultKind = "provider_task"

// errPollAgain is the internal signal that a task or a key is not resolved
// yet and the wait should continue. It never escapes Run.
var errPollAgain = errors.New("providertask: not resolved yet")

// Status is one answer about a task. A failed task carries the provider's
// error code in Code, so a caller can map it onto its own error envelope.
type Status struct {
	// State is where the task stands at the provider.
	State State
	// Progress is the provider's progress report for a running task. Run
	// publishes it on the job's topic unchanged. The zero value publishes
	// nothing.
	Progress job.Progress
	// Code is the provider's error code when State is StateFailed.
	Code string
}

// TaskFailure is the provider's terminal verdict that a task failed. Code
// carries the provider's error code unchanged. errors.As reaches the
// failure and errors.Is matches it against ErrTaskFailed.
type TaskFailure struct {
	// TaskID is the provider task id the verdict names.
	TaskID string
	// Code is the provider's error code, empty when the provider gave none.
	Code string
}

// Error returns the failure as one line. The code comes last, so a log
// line still names the task when the code is empty.
func (f *TaskFailure) Error() string {
	return fmt.Sprintf("providertask: task %s failed: %s", f.TaskID, f.Code)
}

// Unwrap returns ErrTaskFailed, so errors.Is matches every failure the
// provider reported whatever its code.
func (f *TaskFailure) Unwrap() error {
	return ErrTaskFailed
}

// failure returns the TaskFailure for a task id and a provider code.
func failure(taskID, code string) *TaskFailure {
	return &TaskFailure{TaskID: taskID, Code: code}
}

// Claim is one key's storable record. A fresh claim carries no task id and
// StateRunning. Record attaches the task id, and Finish records the
// provider's terminal verdict.
type Claim struct {
	// Key is the caller's idempotency key.
	Key string
	// TaskID is the provider task id, empty while the create is still
	// unrecorded.
	TaskID string
	// State is the recorded verdict, StateRunning while the task is not
	// terminal yet.
	State State
	// Code is the provider's error code recorded by Finish for a failed
	// task.
	Code string
	// CreatedAt is when the key was first claimed.
	CreatedAt time.Time
}

// Store remembers the task id a key created. This package declares the
// interface, and providertask/sqlitestore implements it over SQLite. An
// implementation must be safe for concurrent use, and must return an error
// matching ErrUnknownKey for a key it does not hold.
type Store interface {
	// Claim records key as claimed when the store holds no row for it, and
	// reports the row either way. It reports created true for the one call
	// that inserted the row, so concurrent claims pick one creator. The
	// inserted row carries no task id, StateRunning, and CreatedAt now.
	Claim(ctx context.Context, key string, now time.Time) (Claim, bool, error)
	// Record attaches taskID to the key's row before polling starts. It
	// writes only while the row still carries no task id, so a key never
	// moves to a different task. A repeat with the recorded id is not an
	// error. An unknown key is an error matching ErrUnknownKey.
	Record(ctx context.Context, key, taskID string) error
	// Get returns the row stored under key, or an error matching
	// ErrUnknownKey.
	Get(ctx context.Context, key string) (Claim, error)
	// Finish records the provider's terminal verdict on the key's row.
	// State must be StateSucceeded or StateFailed, and Code carries the
	// provider's error code for a failed task.
	Finish(ctx context.Context, key string, state State, code string) error
	// Release removes the key's row while it still carries no task id, and
	// changes nothing once a task id is recorded. It reports whether it
	// removed a row. Run calls it when a run fails before Create, so a
	// refused reservation does not burn the key. An application calls it
	// after a create it believes never landed, which is the deliberate
	// recovery for a burned key.
	Release(ctx context.Context, key string) (bool, error)
}

// Coordinator serialises concurrent Runs of one key inside one process.
// The Run that joins first drives the task and settles it. The Runs that
// join while it works wait on the store for the recorded outcome, fetch
// the result, and never touch the provider or the meter. Wire one
// Coordinator into every Config of a process, the way one job Runner
// serves every job. A nil Coordinator means the caller guarantees a key
// has at most one Run at a time. Create stays once per key either way,
// because the store claim carries that promise.
type Coordinator struct {
	inFlight sync.Map
}

// NewCoordinator returns a Coordinator ready for Config.
func NewCoordinator() *Coordinator {
	return &Coordinator{}
}

// join registers the key as in flight and reports whether this call leads
// it. The leader drives the task, and every later joiner waits.
func (c *Coordinator) join(key string) bool {
	if c == nil {
		return true
	}
	_, held := c.inFlight.LoadOrStore(key, struct{}{})
	return !held
}

// leave releases the key. Only a leader calls it, and the leader holds the
// entry until its Run returns, so the delete always removes its own entry.
func (c *Coordinator) leave(key string) {
	if c == nil {
		return
	}
	c.inFlight.Delete(key)
}

// Meter is the paid call seam a Run settles through. It matches the shape
// the cost package publishes, so a cost.Meter drives it directly.
type Meter interface {
	// Call runs work as one paid call. It reserves the estimate before the
	// work runs and settles what the work reports.
	Call(ctx context.Context, estimate cost.Price, kind, ref string, work cost.Work) (cost.Usage, error)
}

// meterMatches pins Meter to the seam the cost package publishes, so a
// signature drift breaks the build here rather than at a call site.
var _ Meter = (*cost.Meter)(nil)

// Spec is the provider interaction one Run drives. Create, Status and
// Result are required. The provider client stays in the application, so
// the Spec carries the three calls and nothing else about the provider.
type Spec[T any] struct {
	// Create starts the task at the provider and returns its id. Run calls
	// it at most once per key, ever. An error fails the Run and leaves the
	// key claimed, because the provider may have created the task even
	// though the answer was lost. Later Runs refuse that key with
	// ErrTaskPending rather than create again.
	Create func(ctx context.Context) (string, error)
	// Status asks the provider where the task stands. A returned error is
	// classified by Config.Retryable, so a transient fault waits and a
	// permanent one stops the Run. A Status that reports StateFailed ends
	// the Run with a TaskFailure and is never retried.
	Status func(ctx context.Context, taskID string) (Status, error)
	// Result fetches the task's outcome once it succeeded. A result link
	// expires, so Run calls Result again on every Run that reaches the
	// result, including a resume of a recorded success.
	Result func(ctx context.Context, taskID string) (T, error)
	// Keep copies the result somewhere it outlives the provider's result
	// link, such as a media store. Run calls it before it returns, and
	// once per Run that reaches the result, so a Keep must tolerate
	// repeating. Nil keeps nothing.
	Keep func(ctx context.Context, value T) error
	// Price reports what the finished task cost, measured from the result.
	// The meter settles at the reported price. Nil settles at
	// Config.Estimate. An error fails the Run and frees the reservation,
	// and the row stays open, so a later Run fetches and prices again.
	Price func(ctx context.Context, value T) (cost.Price, error)
}

// Config configures Run. The zero value is usable and carries no meter, no
// retry classification and no progress publication.
type Config struct {
	// Window is how long a recorded running task id is trusted to stay
	// queryable. A recorded row in the running state older than the window
	// refuses with ErrWindowExceeded. A recorded verdict is honoured at any
	// age. Zero or negative means DefaultWindow.
	Window time.Duration
	// Deadline bounds one Run from the claim to the result. The task keeps
	// running at the provider when it passes, and a later Run resumes the
	// recorded task. Zero or negative means DefaultDeadline.
	Deadline time.Duration
	// PollBase is the wait before the second status poll. Each later wait
	// doubles it, up to PollCeiling, and every wait spans half the current
	// step to the whole step. Zero or negative means DefaultPollBase.
	PollBase time.Duration
	// PollCeiling caps one wait between polls. A ceiling below PollBase
	// reads as PollBase. Zero or negative means DefaultPollCeiling.
	PollCeiling time.Duration
	// Retryable reports whether a Status error is a condition a wait can
	// clear. Nil retries nothing, so a forgotten classifier cannot spin on
	// a permanent failure.
	Retryable throttle.Retryable
	// Meter settles the task through the paid call seam. Nil runs the task
	// unmetered, for a provider a caller meters elsewhere or pays nothing
	// for.
	Meter Meter
	// Estimate is the reservation the meter commits before Create, and the
	// price a success settles at when Spec.Price is nil. A failure
	// releases it unless ChargesOnFailure is set.
	Estimate cost.Price
	// Kind names the billed operation on the ledger rows the meter
	// writes. Empty means the package default.
	Kind string
	// ChargesOnFailure reports that the provider charges for a failed task
	// too. A failed task then settles at the estimate instead of freeing
	// the reservation.
	ChargesOnFailure bool
	// Coordinator serialises concurrent Runs of one key in this process.
	// Nil means the caller guarantees a key has at most one Run at a time.
	Coordinator *Coordinator
	// Progress receives one report per published step, ready to hand to
	// the job the Run sits inside. Nil publishes nothing.
	Progress func(job.Progress)
	// Log receives one line per recorded fault, such as a terminal record
	// the store refused. Nil means slog.Default.
	Log *slog.Logger
	// Now supplies the clock that stamps claims and judges the window.
	// Nil means time.Now.
	Now func() time.Time
}

// window returns the query window, substituting the default for an unset
// or negative value.
func (c Config) window() time.Duration {
	if c.Window <= 0 {
		return DefaultWindow
	}
	return c.Window
}

// deadline returns the overall bound, substituting the default for an unset
// or negative value.
func (c Config) deadline() time.Duration {
	if c.Deadline <= 0 {
		return DefaultDeadline
	}
	return c.Deadline
}

// base returns the first poll wait, substituting the default for an unset
// or negative value.
func (c Config) base() time.Duration {
	if c.PollBase <= 0 {
		return DefaultPollBase
	}
	return c.PollBase
}

// ceiling returns the poll wait cap. An unset cap means DefaultPollCeiling,
// and a cap below the first wait reads as the first wait, so the curve
// starts inside its own range.
func (c Config) ceiling() time.Duration {
	limit := c.PollCeiling
	if limit <= 0 {
		limit = DefaultPollCeiling
	}
	if limit < c.base() {
		return c.base()
	}
	return limit
}

// kind returns the billed operation name, substituting the default for an
// empty value.
func (c Config) kind() string {
	if c.Kind == "" {
		return defaultKind
	}
	return c.Kind
}

// nowFunc returns the injected clock, substituting time.Now for nil.
func (c Config) nowFunc() func() time.Time {
	if c.Now == nil {
		return time.Now
	}
	return c.Now
}

// log returns the injected logger, substituting slog.Default for nil.
func (c Config) log() *slog.Logger {
	if c.Log == nil {
		return slog.Default()
	}
	return c.Log
}

// Outcome is what one Run finished with. Value carries the fetched result,
// and Usage reports the price this Run settled through the meter. A Run
// that collected a recorded outcome settles nothing, so its Usage is the
// zero value even when the Run that created the task was metered.
type Outcome[T any] struct {
	// TaskID is the provider task id the key recorded.
	TaskID string
	// Value is the result the provider reported for the task.
	Value T
	// Usage is the price this Run settled, with Measured reporting whether
	// the price came from Spec.Price or from the estimate.
	Usage cost.Usage
}

// isNilValue reports whether v is nil or an interface holding a nil
// pointer. A typed nil store passes a plain == nil check and would panic at
// first use, so the entry check looks at the pointer itself.
func isNilValue(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	return r.Kind() == reflect.Pointer && r.IsNil()
}

// Run carries one provider task from create to result under key. It claims
// the key in store, creates the task only for the claim it inserted, and
// records the task id before it polls. A restart that calls Run again with
// the same key resumes the recorded task and never calls Create again.
//
// A recorded running task older than Config.Window refuses with
// ErrWindowExceeded, while a recorded verdict is honoured at any age.
// A key claimed but never recorded refuses with ErrTaskPending once the
// deadline passes, because the create that claim started may have landed
// at the provider. A task that fails at the provider returns a TaskFailure
// carrying the provider's error code.
//
// With a Meter configured, the Run that creates the task reserves
// Config.Estimate first, settles when the task succeeds, and frees the
// reservation when it fails. A refused reservation releases the still
// empty claim, so the key stays usable. A Run that joins a key another Run
// of the same process is already driving waits for the recorded outcome
// and settles nothing. That joiner refuses with ErrTaskPending when its
// own deadline passes first.
//
// The returned error wraps the sentinels of this package, the meter's
// errors, and the errors the Spec functions returned. A store fault on the
// terminal record is logged and does not fail an otherwise finished Run,
// because the provider verdict and the settled charge are already real.
func Run[T any](ctx context.Context, cfg Config, store Store, key string, spec Spec[T]) (Outcome[T], error) {
	if err := validate(cfg, store, key, spec); err != nil {
		return Outcome[T]{}, err
	}
	t := &task[T]{
		cfg:   cfg,
		store: store,
		key:   key,
		spec:  spec,
		now:   cfg.nowFunc(),
	}
	// The provider calls run under the deadline. The store writes run on a
	// context that outlives it, so a deadline never stops the claim, the
	// record or the terminal verdict from landing.
	pctx, cancel := context.WithTimeout(ctx, cfg.deadline())
	defer cancel()
	t.pctx = pctx
	t.wctx = context.WithoutCancel(pctx)

	if !cfg.Coordinator.join(key) {
		// Another Run of this process is driving the key, so this one
		// waits for the recorded outcome and settles nothing. The leader
		// releases the key when it returns, so a joiner leaves nothing.
		return t.await()
	}
	defer cfg.Coordinator.leave(key)

	claim, created, err := store.Claim(t.wctx, key, t.now())
	if err != nil {
		return Outcome[T]{}, fmt.Errorf("providertask: claim %s: %w", key, err)
	}
	if created {
		return t.lead()
	}
	if claim.TaskID == "" {
		// Another process claimed the key and has not recorded a task id.
		// The create may still land, so this Run waits rather than creates
		// again.
		return t.await()
	}
	return t.resume(claim)
}

// validate refuses a Run this package cannot drive. A typed nil store is
// refused like a plain nil, because a store that cannot remember would
// only pretend to keep the resume promise.
func validate[T any](cfg Config, store Store, key string, spec Spec[T]) error {
	if isNilValue(store) {
		return fmt.Errorf("providertask: run: %w: nil store", ErrInvalid)
	}
	if key == "" {
		return fmt.Errorf("providertask: run: %w: empty key", ErrInvalid)
	}
	if spec.Create == nil || spec.Status == nil || spec.Result == nil {
		return fmt.Errorf("providertask: run: %w: spec needs create, status and result", ErrInvalid)
	}
	return nil
}

// task is one Run in flight. It carries the config, the store, the key and
// the spec, plus the two contexts the Run works under.
type task[T any] struct {
	cfg   Config
	store Store
	key   string
	spec  Spec[T]
	now   func() time.Time
	// pctx bounds the provider calls with the overall deadline.
	pctx context.Context
	// wctx carries the store writes past the deadline and past a caller
	// that gives up, so the durable record never waits on a live request.
	wctx context.Context
}

// lead drives the Run that inserted the claim. It creates the task,
// records the id before polling, and settles through the meter when one is
// configured.
func (t *task[T]) lead() (Outcome[T], error) {
	taskID, value, usage, err := t.metered(t.createWatch)
	return t.land(taskID, value, usage, err)
}

// resume drives a Run that leads a key whose task id is recorded. The Run
// that recorded it has ended, either because its process restarted or
// because its deadline passed, so this Run owns the settle. A recorded
// verdict is honoured at any age, because the provider already answered
// it, and only the live poll faces the query window.
func (t *task[T]) resume(claim Claim) (Outcome[T], error) {
	switch claim.State {
	case StateSucceeded:
		return t.collect(claim)
	case StateFailed:
		return Outcome[T]{}, failure(claim.TaskID, claim.Code)
	}
	if err := t.checkWindow(claim); err != nil {
		return Outcome[T]{}, err
	}
	taskID, value, usage, err := t.metered(func() (string, T, cost.Usage, error) {
		v, u, werr := t.watch(claim.TaskID)
		return claim.TaskID, v, u, werr
	})
	return t.land(taskID, value, usage, err)
}

// metered drives work through the meter when one is configured. The meter
// reserves the estimate before the work runs and settles what it reports.
// A provider verdict of failed settles at the estimate instead of freeing
// the reservation when ChargesOnFailure holds, and the verdict surfaces
// once the meter is done. A fault before the work ran is a refused
// reservation, and the still empty claim is released, so the key stays
// usable. The settled usage is what the outcome carries.
func (t *task[T]) metered(work func() (string, T, cost.Usage, error)) (string, T, cost.Usage, error) {
	if isNilValue(t.cfg.Meter) {
		return work()
	}
	var (
		taskID  string
		value   T
		fail    *TaskFailure
		entered bool
	)
	settled, err := t.cfg.Meter.Call(t.pctx, t.cfg.Estimate, t.cfg.kind(), t.key, func(context.Context) (cost.Usage, error) {
		entered = true
		id, v, u, werr := work()
		taskID, value = id, v
		if werr == nil {
			return u, nil
		}
		if errors.As(werr, &fail) && t.cfg.ChargesOnFailure {
			return cost.Usage{Measured: false}, nil
		}
		return cost.Usage{}, werr
	})
	if err != nil {
		if !entered {
			// The reservation was refused, so nothing was created and the
			// claim still carries no task id. The release changes nothing
			// once a task id is recorded.
			t.releaseEmptyClaim()
		}
		var zero T
		return taskID, zero, cost.Usage{}, err
	}
	if fail != nil {
		var zero T
		return taskID, zero, settled, fail
	}
	return taskID, value, settled, nil
}

// createWatch creates the task, records the id and watches it to its
// result. The task id comes back even when a later step failed, so the
// caller can tell a claim the create reached from one it did not.
func (t *task[T]) createWatch() (string, T, cost.Usage, error) {
	taskID, err := t.create()
	if err != nil {
		var zero T
		return "", zero, cost.Usage{}, err
	}
	if err := t.store.Record(t.wctx, t.key, taskID); err != nil {
		var zero T
		return taskID, zero, cost.Usage{}, fmt.Errorf("providertask: record %s: %w", t.key, err)
	}
	t.publish(job.Progress{Stage: StageCreated})
	value, usage, err := t.watch(taskID)
	if err != nil {
		return taskID, value, cost.Usage{}, err
	}
	return taskID, value, usage, nil
}

// land finishes a Run that drove a task to a verdict. It records the
// verdict, keeps the result and returns the outcome. The verdict lands
// before Keep, so a crash after it never re-settles a later resume, and a
// Keep failure leaves a row a later Run can collect from without paying
// again. Any other fault passes through.
func (t *task[T]) land(taskID string, value T, usage cost.Usage, err error) (Outcome[T], error) {
	var fail *TaskFailure
	if err != nil && errors.As(err, &fail) {
		if ferr := t.store.Finish(t.wctx, t.key, StateFailed, fail.Code); ferr != nil {
			t.cfg.log().Warn("providertask: record verdict", "key", t.key, "error", ferr)
		}
		return Outcome[T]{}, fail
	}
	if err != nil {
		return Outcome[T]{}, err
	}
	if ferr := t.store.Finish(t.wctx, t.key, StateSucceeded, ""); ferr != nil {
		t.cfg.log().Warn("providertask: record verdict", "key", t.key, "error", ferr)
	}
	if t.spec.Keep != nil {
		if kerr := t.spec.Keep(t.pctx, value); kerr != nil {
			return Outcome[T]{}, fmt.Errorf("providertask: keep %s: %w", taskID, kerr)
		}
	}
	return Outcome[T]{TaskID: taskID, Value: value, Usage: usage}, nil
}

// releaseEmptyClaim removes the key's row while it still carries no task
// id. A fault is logged, because the caller already holds the refusal that
// caused the release.
func (t *task[T]) releaseEmptyClaim() {
	if _, err := t.store.Release(t.wctx, t.key); err != nil {
		t.cfg.log().Warn("providertask: release claim", "key", t.key, "error", err)
	}
}

// create calls the spec's Create and refuses an empty task id, because a
// create that names no task cannot be resumed or polled.
func (t *task[T]) create() (string, error) {
	taskID, err := t.spec.Create(t.pctx)
	if err != nil {
		return "", fmt.Errorf("providertask: create %s: %w", t.key, err)
	}
	if taskID == "" {
		return "", fmt.Errorf("providertask: create %s: %w: empty task id", t.key, ErrInvalid)
	}
	return taskID, nil
}

// watch polls the task to a terminal state and fetches the result. The
// usage carries the measured price when the spec prices the result, and a
// provider verdict of failed comes back as the error.
func (t *task[T]) watch(taskID string) (T, cost.Usage, error) {
	if _, err := t.poll(taskID); err != nil {
		var zero T
		return zero, cost.Usage{}, err
	}
	value, err := t.spec.Result(t.pctx, taskID)
	if err != nil {
		var zero T
		return zero, cost.Usage{}, fmt.Errorf("providertask: result %s: %w", taskID, err)
	}
	if t.spec.Price == nil {
		return value, cost.Usage{}, nil
	}
	price, err := t.spec.Price(t.pctx, value)
	if err != nil {
		var zero T
		return zero, cost.Usage{}, fmt.Errorf("providertask: price %s: %w", taskID, err)
	}
	return value, cost.Usage{Price: price, Measured: true}, nil
}

// poll asks the provider where the task stands until it reaches a terminal
// state. Each wait spans half the current step to the whole step, on the
// curve from the base to the ceiling, and the overall deadline bounds the
// whole loop. A Status error the classifier
// calls transient is retried on that curve. A failed verdict and a
// permanent error stop the loop at once.
func (t *task[T]) poll(taskID string) (Status, error) {
	var verdict Status
	err := throttle.Retry(t.pctx, t.pollCfg(), t.pollRetryable(), func() error {
		st, err := t.spec.Status(t.pctx, taskID)
		if err != nil {
			return err
		}
		t.publish(st.Progress)
		switch st.State {
		case StateRunning:
			return errPollAgain
		case StateSucceeded:
			verdict = st
			return nil
		case StateFailed:
			return failure(taskID, st.Code)
		default:
			return fmt.Errorf("providertask: status %s: unknown state %q", taskID, st.State)
		}
	})
	if err == nil {
		return verdict, nil
	}
	var fail *TaskFailure
	if errors.As(err, &fail) {
		return Status{}, fail
	}
	if errors.Is(err, errPollAgain) {
		if errors.Is(t.pctx.Err(), context.DeadlineExceeded) {
			return Status{}, fmt.Errorf("providertask: task %s: %w", taskID, ErrDeadline)
		}
		return Status{}, t.pctx.Err()
	}
	if errors.Is(t.pctx.Err(), context.DeadlineExceeded) {
		return Status{}, fmt.Errorf("providertask: task %s: %w", taskID, ErrDeadline)
	}
	return Status{}, fmt.Errorf("providertask: status %s: %w", taskID, err)
}

// pollCfg paces the polling and waiting loops. The attempt budget is
// unbounded, because the overall deadline is the bound that matters, and a
// provider task may run for many backoff periods.
func (t *task[T]) pollCfg() throttle.Config {
	return throttle.Config{
		Attempts: math.MaxInt,
		Backoff:  t.cfg.base(),
		Max:      t.cfg.ceiling(),
	}
}

// pollRetryable classifies the errors the polling loop waits on. The
// not-resolved-yet signal always waits. A provider verdict of failed never
// waits, whatever the caller's classifier says about it. Everything else
// is the caller's call, and a nil classifier retries nothing.
func (t *task[T]) pollRetryable() throttle.Retryable {
	return func(err error) bool {
		if errors.Is(err, errPollAgain) {
			return true
		}
		if errors.Is(err, ErrTaskFailed) {
			return false
		}
		if t.cfg.Retryable == nil {
			return false
		}
		return t.cfg.Retryable(err)
	}
}

// collect fetches the result of a recorded success and keeps it. No meter
// runs here, because the Run that reached the verdict settled the task.
func (t *task[T]) collect(claim Claim) (Outcome[T], error) {
	value, err := t.spec.Result(t.pctx, claim.TaskID)
	if err != nil {
		return Outcome[T]{}, fmt.Errorf("providertask: result %s: %w", claim.TaskID, err)
	}
	if t.spec.Keep != nil {
		if err := t.spec.Keep(t.pctx, value); err != nil {
			return Outcome[T]{}, fmt.Errorf("providertask: keep %s: %w", claim.TaskID, err)
		}
	}
	return Outcome[T]{TaskID: claim.TaskID, Value: value}, nil
}

// checkWindow refuses a claim whose recorded age passed the query window.
// Only a claim that still runs faces the window, because the provider may
// no longer answer queries about the task. A recorded verdict is never
// refused, because the provider already answered it.
func (t *task[T]) checkWindow(claim Claim) error {
	age := t.now().Sub(claim.CreatedAt)
	if age <= t.cfg.window() {
		return nil
	}
	return fmt.Errorf("providertask: key %s recorded %s ago: %w", t.key, age.Round(time.Second), ErrWindowExceeded)
}

// await waits for a key another Run holds. The other Run may be creating
// the task right now or polling it, so this Run reads the store only and
// never touches the provider or the meter. A row that reaches a recorded
// verdict resolves the wait. A row past the window stops the wait, because
// the verdict that claim is heading for will refuse the same way.
func (t *task[T]) await() (Outcome[T], error) {
	var resolved Claim
	err := throttle.Retry(t.pctx, t.pollCfg(), func(err error) bool {
		return errors.Is(err, errPollAgain)
	}, func() error {
		claim, err := t.store.Get(t.pctx, t.key)
		if err != nil {
			if errors.Is(err, ErrUnknownKey) {
				// The claim has not landed yet, so the wait continues.
				return errPollAgain
			}
			return fmt.Errorf("providertask: get %s: %w", t.key, err)
		}
		switch claim.State {
		case StateSucceeded, StateFailed:
			resolved = claim
			return nil
		default:
			if werr := t.checkWindow(claim); werr != nil {
				return werr
			}
			return errPollAgain
		}
	})
	if err == nil {
		if resolved.State == StateFailed {
			return Outcome[T]{}, failure(resolved.TaskID, resolved.Code)
		}
		return t.collect(resolved)
	}
	if errors.Is(err, errPollAgain) || errors.Is(t.pctx.Err(), context.DeadlineExceeded) {
		return Outcome[T]{}, fmt.Errorf("providertask: key %s: %w", t.key, ErrTaskPending)
	}
	return Outcome[T]{}, err
}

// publish hands one progress report to the config's hook. An empty report
// is dropped, because an eventless frame is wire noise.
func (t *task[T]) publish(p job.Progress) {
	if t.cfg.Progress == nil {
		return
	}
	if p.Stage == "" && p.Current == nil && p.Total == nil && len(p.Detail) == 0 {
		return
	}
	t.cfg.Progress(p)
}
