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
// A provider task is created once and settled once, across processes,
// including after a crash or a leader stall past its lease. One process
// drives a recorded task at a time. Another takes over only after the
// driver's lease expires or is released.
//
// Every claim carries a lease. The driving Run renews its lease on a timer
// of its own, at about a third of the lease length, so a slow create never
// costs it the drive. The lease length is about three poll ceilings, with
// a thirty second floor. A Run that finds a recorded task takes the lease
// before it resumes. A Run whose own deadline passes without a verdict
// releases the lease, so the next Run takes over without waiting for
// expiry. A waiter tries the takeover itself whenever the lease it waits
// on expires mid wait. A driver checks its token immediately before it
// settles, and Finish refuses a token the row no longer carries. A leader
// that wakes past its lease therefore neither books twice nor overwrites
// the verdict. Takeover compares timestamps, so processes sharing a store
// must share a clock, or keep their skew well below the lease length.
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
// terminal record are two writes. Run settles each key at most once
// through the meter's once option, so a resume after a crash between them
// books nothing, and the charge the first settle booked stands. The
// provider billed once either way.
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
	"sync/atomic"
	"time"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/id"
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
	// ErrStaleToken reports a lease write that named a token the row no
	// longer carries. The caller that sent it stopped driving the task
	// before the write landed. Finish refuses a stale token, so a leader
	// that woke past its lease never overwrites the verdict the new owner
	// recorded.
	ErrStaleToken = errors.New("providertask: lease token is stale")
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

// errOwnershipLost is the internal signal that this Run lost the lease it
// drove under, so another process owns the settle now. A Run that receives
// it stops driving and falls back to awaiting the recorded outcome, like a
// joiner. It never escapes Run.
var errOwnershipLost = errors.New("providertask: lease lost mid drive")

// ownerID names this process in lease rows. It is one random id per
// process, drawn at startup. Every Run in the process claims and renews
// under the same name, so a takeover the process wins counts as self.
var (
	ownerID    string
	ownerIDErr error
)

// init draws the process's driver id at startup. An entropy failure is
// stored for the first Run instead of a panic, because a library returns
// errors.
func init() {
	ownerID, ownerIDErr = id.New()
}

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

// Claim is one key's storable record. A fresh claim carries no task id,
// StateRunning, and the lease of the Run that inserted it. Record attaches
// the task id, and Finish records the provider's terminal verdict.
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
	// Owner names the process that holds the lease. Empty names no driver,
	// which is the state Finish and ReleaseLease leave behind.
	Owner string
	// Token is the lease's fencing number. It starts at one on creation and
	// increments on every takeover, so a write naming an older token
	// refuses.
	Token int64
	// LeaseUntil is the instant the holder's lease expires. A zero value
	// names no live lease, so the next takeover wins at once.
	LeaseUntil time.Time
}

// Store remembers the task id a key created and which process is driving
// it. This package declares the interface, and providertask/sqlitestore
// implements it over SQLite. An implementation must be safe for concurrent
// use, and must return an error matching ErrUnknownKey for a key it does
// not hold. The lease writes compare timestamps against the caller's now,
// so an implementation stores them as given and shifts nothing.
type Store interface {
	// Claim records key as claimed when the store holds no row for it, and
	// reports the row either way. It reports created true for the one call
	// that inserted the row, so concurrent claims pick one creator. The
	// inserted row carries no task id, StateRunning, CreatedAt now, and a
	// lease naming owner with token one and the expiry now plus ttl.
	Claim(ctx context.Context, key, owner string, now time.Time, ttl time.Duration) (Claim, bool, error)
	// TakeOver transfers the lease of key to owner when it is takeable,
	// which is when the stored lease expired before now or already names
	// owner. It bumps the token, so every write the previous holder still
	// sends refuses as stale, and reports the new token. It reports ok
	// false without changing the row when another driver still holds an
	// unexpired lease. An unknown key is an error matching ErrUnknownKey.
	TakeOver(ctx context.Context, key, owner string, now time.Time, ttl time.Duration) (token int64, ok bool, err error)
	// Renew extends the lease of key to until when the row still names
	// owner and carries token. It reports ok false when they do not, which
	// means the caller lost the drive.
	Renew(ctx context.Context, key, owner string, token int64, until time.Time) (ok bool, err error)
	// ReleaseLease clears the lease of key when the row still names owner
	// and carries token, so the next Run takes over without waiting for
	// expiry. It changes nothing when they do not match.
	ReleaseLease(ctx context.Context, key, owner string, token int64) error
	// Record attaches taskID to the key's row before polling starts. It
	// writes only while the row still carries no task id, so a key never
	// moves to a different task. A repeat with the recorded id is not an
	// error. An unknown key is an error matching ErrUnknownKey.
	Record(ctx context.Context, key, taskID string) error
	// Get returns the row stored under key, or an error matching
	// ErrUnknownKey.
	Get(ctx context.Context, key string) (Claim, error)
	// Finish records the provider's terminal verdict on the key's row and
	// clears its lease. State must be StateSucceeded or StateFailed, and
	// Code carries the provider's error code for a failed task. It refuses
	// with an error matching ErrStaleToken when the row no longer carries
	// token. A driver that lost its lease never overwrites the verdict the
	// new owner recorded. An unknown key is an error matching
	// ErrUnknownKey.
	Finish(ctx context.Context, key string, token int64, state State, code string) error
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
// the cost package publishes, so a cost.Meter drives it directly. Run
// always passes the once option, so a meter backed by a cost.Meter settles
// a key at most once per kind and reference.
type Meter interface {
	// Call runs work as one paid call. It reserves the estimate before the
	// work runs and settles what the work reports. The options carry the
	// cost package's call options, and Run passes them through.
	Call(ctx context.Context, estimate cost.Price, kind, ref string, work cost.Work, opts ...cost.CallOption) (cost.Usage, error)
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
	// The lease renewal reads it from a goroutine of its own, so it must
	// be safe for concurrent use. Nil means time.Now.
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

// minLease is the floor under the computed lease length, so a driver whose
// provider answers quickly still holds its task long enough for a
// replacement process to reach a takeover.
const minLease = 30 * time.Second

// lease returns how long one drive holds its key before another process may
// take it over. It is about three poll ceilings, because a driver that
// polls at ceiling pace renews twice inside one lease even when a poll or a
// create runs long. It never drops below minLease.
func (c Config) lease() time.Duration {
	if span := 3 * c.ceiling(); span > minLease {
		return span
	}
	return minLease
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
// The drive holds a lease. Run that finds a recorded task takes the lease
// over before it resumes, and waits like a joiner when another process
// still holds it. A Run whose deadline passes without a verdict releases
// the lease, and Finish clears it, so the next Run takes over at once. The
// driver renews the lease on a timer of its own and checks its token
// immediately before it settles. A leader stalled past its lease therefore
// neither books twice nor overwrites the verdict of the process that took
// over.
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
	if ownerIDErr != nil {
		return Outcome[T]{}, fmt.Errorf("providertask: run %s: %w", key, ownerIDErr)
	}
	t := &task[T]{
		cfg:   cfg,
		store: store,
		key:   key,
		spec:  spec,
		now:   cfg.nowFunc(),
		owner: ownerID,
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

	claim, created, err := store.Claim(t.wctx, key, t.owner, t.now(), t.cfg.lease())
	if err != nil {
		return Outcome[T]{}, fmt.Errorf("providertask: claim %s: %w", key, err)
	}
	if created {
		return t.joinOnLost(t.lead(claim.Token))
	}
	if claim.TaskID == "" {
		// Another process claimed the key and has not recorded a task id.
		// The create may still land, so this Run waits rather than creates
		// again.
		return t.await()
	}
	token, ok, err := store.TakeOver(t.wctx, key, t.owner, t.now(), t.cfg.lease())
	if err != nil {
		return Outcome[T]{}, fmt.Errorf("providertask: take over %s: %w", key, err)
	}
	if !ok {
		// Another process holds an unexpired lease on the recorded task.
		// This Run waits like an in-process joiner: no Status calls, no
		// meter, and it collects the result once one is recorded.
		return t.await()
	}
	return t.joinOnLost(t.resume(claim, token))
}

// joinOnLost turns a drive that lost its lease mid way into a wait for the
// recorded outcome, because the process that won the lease settles now.
func (t *task[T]) joinOnLost(outcome Outcome[T], err error) (Outcome[T], error) {
	if errors.Is(err, errOwnershipLost) {
		return t.await()
	}
	return outcome, err
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
// the spec, plus the contexts the Run works under and the lease it drives
// under.
type task[T any] struct {
	cfg   Config
	store Store
	key   string
	spec  Spec[T]
	now   func() time.Time
	// owner is this process's driver id. Every lease this Run writes names
	// it, and a takeover the process wins counts as self.
	owner string
	// pctx bounds the provider calls with the overall deadline.
	pctx context.Context
	// wctx carries the store writes past the deadline and past a caller
	// that gives up, so the durable record never waits on a live request.
	wctx context.Context
	// dctx bounds the poll phase. drive sets it, and a failed renewal
	// cancels it, which stops a driver that lost its lease.
	dctx context.Context
	// token is the fencing number of the lease this drive holds. drive
	// sets it, and holdLease reads it immediately before a settle.
	token int64
	// lostLease reports that the renewal failed, so a poll stopped by the
	// cancellation above reports the loss instead of a bare context error.
	// The renewal goroutine sets it and the drive reads it.
	lostLease atomic.Bool
}

// lead drives the Run that inserted the claim. It creates the task,
// records the id before polling, and settles through the meter when one is
// configured.
func (t *task[T]) lead(token int64) (Outcome[T], error) {
	taskID, value, usage, err := t.drive(token, t.createWatch)
	return t.land(taskID, value, usage, err, token)
}

// resume drives a Run that won the lease of a key whose task id is
// recorded. The Run that recorded it stopped driving, either because its
// process died or because its deadline passed, so this Run owns the
// settle. A recorded verdict is honoured at any age, because the provider
// already answered it, and only the live poll faces the query window.
func (t *task[T]) resume(claim Claim, token int64) (Outcome[T], error) {
	switch claim.State {
	case StateSucceeded:
		return t.collect(claim)
	case StateFailed:
		return Outcome[T]{}, failure(claim.TaskID, claim.Code)
	}
	if err := t.checkWindow(claim); err != nil {
		return Outcome[T]{}, err
	}
	taskID, value, usage, err := t.drive(token, func() (string, T, cost.Usage, error) {
		v, u, werr := t.watch(claim.TaskID)
		return claim.TaskID, v, u, werr
	})
	return t.land(taskID, value, usage, err, token)
}

// drive runs one paid-call body under the lease token. A renewal loop
// extends the lease on a timer of its own while the work runs, because a
// single create or poll can outlast a renewal interval. A renewal that
// fails cancels the poll phase and ends the drive with errOwnershipLost,
// so the caller falls back to awaiting the recorded outcome the new owner
// will produce. The renewal goroutine exits when the drive stops it, or
// after its own failed renewal.
func (t *task[T]) drive(token int64, work func() (string, T, cost.Usage, error)) (string, T, cost.Usage, error) {
	t.token = token
	t.lostLease.Store(false)
	dctx, cancel := context.WithCancel(t.pctx)
	t.dctx = dctx
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(t.cfg.lease() / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				ok, err := t.store.Renew(t.wctx, t.key, t.owner, token, t.now().Add(t.cfg.lease()))
				if err != nil || !ok {
					// The row refused the extension, so another driver
					// owns the settle now. Stop the poll phase and let
					// the drive fall back to a joiner.
					t.lostLease.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		close(stop)
		cancel()
	}()
	taskID, value, usage, err := t.metered(work)
	if err != nil && t.lostLease.Load() {
		// A renewal failed while the work ran, so the fault below is the
		// lost drive and not the provider's answer.
		var zero T
		return taskID, zero, cost.Usage{}, errOwnershipLost
	}
	return taskID, value, usage, err
}

// holdLease reports whether this Run still drives the key under token. The
// check runs immediately before a settle, so a leader that lost its lease
// while it worked never books alongside the new owner. The SettleOnce the
// meter settles with stays as the backstop for the window between this
// check and the booking.
func (t *task[T]) holdLease(token int64) bool {
	claim, err := t.store.Get(t.wctx, t.key)
	if err != nil {
		return false
	}
	return claim.Owner == t.owner && claim.Token == token
}

// releaseLease gives the key back when the drive ended without a verdict,
// so the next Run takes over without waiting for expiry. A fault is
// logged, because the lease expires on its own and a late release only
// delays the takeover.
func (t *task[T]) releaseLease(token int64) {
	if err := t.store.ReleaseLease(t.wctx, t.key, t.owner, token); err != nil {
		t.cfg.log().Warn("providertask: release lease", "key", t.key, "error", err)
	}
}

// metered drives work through the meter when one is configured. The meter
// reserves the estimate before the work runs and settles what it reports.
// The settle carries the once option under the key as the reference, so a
// resume of a settled key books nothing. A provider verdict of failed
// settles at the estimate instead of freeing the reservation when
// ChargesOnFailure holds, and the verdict surfaces once the meter is done.
// A fault before the work ran is a refused reservation, and the still empty
// claim is released, so the key stays usable. The settled usage is what the
// outcome carries.
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
	}, cost.Once())
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

// isDeadline reports whether a drive fault is the Run's own deadline. The
// poll loop raises it as ErrDeadline, and the create, result and price
// phases surface it as the context's own error.
func isDeadline(err error) bool {
	return errors.Is(err, ErrDeadline) || errors.Is(err, context.DeadlineExceeded)
}

// land finishes a Run that drove a task to a verdict. It records the
// verdict, keeps the result and returns the outcome. The verdict lands
// before Keep, so a crash after it never re-settles a later resume, and a
// Keep failure leaves a row a later Run can collect from without paying
// again. A drive that reached its deadline without a verdict releases the
// lease, so the next Run takes over without waiting for expiry. Any other
// fault passes through.
func (t *task[T]) land(taskID string, value T, usage cost.Usage, err error, token int64) (Outcome[T], error) {
	var fail *TaskFailure
	if err != nil && errors.As(err, &fail) {
		if ferr := t.store.Finish(t.wctx, t.key, token, StateFailed, fail.Code); ferr != nil {
			t.cfg.log().Warn("providertask: record verdict", "key", t.key, "error", ferr)
		}
		return Outcome[T]{}, fail
	}
	if err != nil {
		if isDeadline(err) {
			t.releaseLease(token)
		}
		return Outcome[T]{}, err
	}
	if ferr := t.store.Finish(t.wctx, t.key, token, StateSucceeded, ""); ferr != nil {
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
// provider verdict of failed comes back as the error. The lease is checked
// immediately before the usage is returned, because the settle follows it,
// so a driver that lost its lease mid poll books nothing.
func (t *task[T]) watch(taskID string) (T, cost.Usage, error) {
	if _, err := t.poll(taskID); err != nil {
		var zero T
		return zero, cost.Usage{}, err
	}
	value, err := t.spec.Result(t.dctx, taskID)
	if err != nil {
		var zero T
		return zero, cost.Usage{}, fmt.Errorf("providertask: result %s: %w", taskID, err)
	}
	if t.spec.Price == nil {
		if !t.holdLease(t.token) {
			var zero T
			return zero, cost.Usage{}, errOwnershipLost
		}
		return value, cost.Usage{}, nil
	}
	price, err := t.spec.Price(t.dctx, value)
	if err != nil {
		var zero T
		return zero, cost.Usage{}, fmt.Errorf("providertask: price %s: %w", taskID, err)
	}
	if !t.holdLease(t.token) {
		// Another process took the lease while this Run worked, so the new
		// owner settles the task. This Run books nothing and awaits the
		// recorded outcome.
		var zero T
		return zero, cost.Usage{}, errOwnershipLost
	}
	return value, cost.Usage{Price: price, Measured: true}, nil
}

// poll asks the provider where the task stands until it reaches a terminal
// state. Each wait spans half the current step to the whole step, on the
// curve from the base to the ceiling, and the overall deadline bounds the
// whole loop. The poll phase runs under the drive context, so a failed
// renewal stops it at once. A Status error the classifier
// calls transient is retried on that curve. A failed verdict and a
// permanent error stop the loop at once.
func (t *task[T]) poll(taskID string) (Status, error) {
	pollCtx := t.pctx
	if t.dctx != nil {
		pollCtx = t.dctx
	}
	var verdict Status
	err := throttle.Retry(pollCtx, t.pollCfg(), t.pollRetryable(), func() error {
		st, err := t.spec.Status(pollCtx, taskID)
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
// never touches the meter. A row that reaches a recorded verdict resolves
// the wait. A recorded task whose lease expires mid wait is takeable,
// because the driver that held it stopped renewing, and this Run drives it
// like any resume. A key that still carries no task id keeps waiting even
// past its lease, because the create that claim started may still land and
// a second create is a second charge. A row past the window stops the
// wait, because the verdict that claim is heading for will refuse the same
// way.
func (t *task[T]) await() (Outcome[T], error) {
	for {
		var resolved Claim
		var (
			token int64
			took  bool
		)
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
				if claim.TaskID == "" || claim.LeaseUntil.After(t.now()) {
					return errPollAgain
				}
				// The lease expired under a recorded task, so this waiter
				// tries the takeover. A dead leader never strands its
				// waiters until their own deadline.
				tok, ok, terr := t.store.TakeOver(t.pctx, t.key, t.owner, t.now(), t.cfg.lease())
				if terr != nil {
					return fmt.Errorf("providertask: take over %s: %w", t.key, terr)
				}
				if !ok {
					// Another waiter won the expired lease first, so the
					// wait continues.
					return errPollAgain
				}
				resolved = claim
				token, took = tok, true
				return nil
			}
		})
		if err != nil {
			if errors.Is(err, errPollAgain) || errors.Is(t.pctx.Err(), context.DeadlineExceeded) {
				return Outcome[T]{}, fmt.Errorf("providertask: key %s: %w", t.key, ErrTaskPending)
			}
			return Outcome[T]{}, err
		}
		if took {
			// This Run owns the drive now, exactly as a resume does.
			outcome, derr := t.resume(resolved, token)
			if errors.Is(derr, errOwnershipLost) {
				// The drive this waiter won was taken over again
				// underneath it, so the wait starts over.
				continue
			}
			return outcome, derr
		}
		if resolved.State == StateFailed {
			return Outcome[T]{}, failure(resolved.TaskID, resolved.Code)
		}
		return t.collect(resolved)
	}
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
