// Package outbox holds events durably on this machine and replays them to a
// sink the application supplies until the sink acknowledges them.
//
// An app that ships events to a remote collector writes each event here
// first. The write is durable before Add returns, so a crash between the
// write and the next delivery pass loses nothing. A delivery pass hands the
// pending entries to the sink in insertion order and retires a batch only
// after the sink accepted all of it.
//
// Delivery is at-least-once from this side. A crash after the sink accepted
// a batch but before the batch was retired replays it. At-most-once needs
// the sink to check the entry id, which Add assigns and which stays stable
// across every replay, and to drop an id it has served.
//
// An entry whose attempts are spent stays stored with its recorded failure
// count. Exhausted reports those entries, so a sink that never accepts one
// entry is a fact a caller can see rather than a queue that silently stops.
//
// The outbox is local and carries no transport. No remote coordination
// exists beyond the entry id. The sink is caller code, so a warehouse
// loader, an HTTP collector, or an object writer is one method away.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/nrynss/keel/id"
)

// Sentinel errors. Open refuses an outbox that could not remember or could
// not deliver, and every refusal wraps one of these.
var (
	// ErrNoStore is returned by Open when no store is configured.
	ErrNoStore = errors.New("outbox: nil store")

	// ErrNoSink is returned by Open when no sink is configured.
	ErrNoSink = errors.New("outbox: nil sink")
)

// DefaultBatchSize is the largest number of entries one Deliver call
// carries when Config.BatchSize is unset.
const DefaultBatchSize = 100

// DefaultInterval is how long Loop waits between passes when Config.Interval
// is unset.
const DefaultInterval = time.Second

// DefaultMaxAttempts is how many failed attempts an entry may record before
// the queue reports it as exhausted when Config.MaxAttempts is unset.
const DefaultMaxAttempts = 10

// DefaultRetryWait is the wait Loop holds after a failed pass when
// Config.RetryWait is unset. Each further consecutive failed pass doubles
// the wait, up to DefaultRetryMax.
const DefaultRetryWait = 5 * time.Second

// DefaultRetryMax caps the retry wait when Config.RetryMax is unset.
const DefaultRetryMax = time.Minute

// Entry is one event waiting to be delivered. Add returns one when it is
// stored, and the sink receives them in batches.
type Entry struct {
	// ID is the stable identifier Add assigned. It is unchanged across
	// every replay, so a sink that records the ids it has served can drop
	// a repeat.
	ID string

	// Payload is the event bytes. The outbox passes them to the sink
	// unchanged.
	Payload []byte

	// Failures is the number of delivery attempts that ended in an error.
	// A fresh entry carries zero.
	Failures int

	// AddedAt is the moment Add recorded the entry.
	AddedAt time.Time
}

// Sink is the remote side one delivery pass hands entries to. The
// application writes it, because the transport belongs to the caller. The
// outbox calls Deliver from one pass at a time.
type Sink interface {
	// Deliver delivers one batch and returns nil only when every entry in
	// it landed. A sink that delivered part of the batch and then failed
	// still reports the error, so the whole batch replays. That replay is
	// why delivery is at-least-once and why the entry id is the repeat
	// guard a strict sink checks.
	Deliver(ctx context.Context, batch []Entry) error
}

// Store remembers entries durably. This package declares the interface, and
// outbox/sqlitestore implements it over SQLite. An implementation must be
// safe for concurrent use, because Add and Flush drive it at the same time.
type Store interface {
	// Add appends e before it returns, so a crash after Add returns
	// cannot lose the entry. The outbox assigns ID, Failures and AddedAt
	// before the call.
	Add(ctx context.Context, e Entry) error

	// Pending returns at most limit entries still owed whose recorded
	// failures are fewer than maxFailures, oldest first. A pass replays
	// them in this order. A negative limit removes the bound, and zero
	// returns nothing.
	Pending(ctx context.Context, maxFailures, limit int) ([]Entry, error)

	// Exhausted returns every entry still owed whose recorded failures
	// have reached maxFailures, oldest first.
	Exhausted(ctx context.Context, maxFailures int) ([]Entry, error)

	// Delivered retires the named entries. The outbox calls it once, and
	// only after the sink accepted the batch they came in. A name that is
	// already retired is not an error.
	Delivered(ctx context.Context, ids []string) error

	// RecordFailures adds one recorded failure to each named entry. The
	// outbox calls it once, after Deliver refused the batch they came in.
	RecordFailures(ctx context.Context, ids []string) error
}

// Summary reports what one delivery pass did.
type Summary struct {
	// Delivered is the number of entries the sink accepted during the
	// pass.
	Delivered int
}

// Config configures Open. The zero value is usable except that Store and
// Sink must be set, because an outbox that could not remember or could not
// deliver would only pretend to.
type Config struct {
	// Store holds the entries. It must not be nil.
	Store Store

	// Sink receives the batches. It must not be nil.
	Sink Sink

	// BatchSize is the largest number of entries one Deliver call carries.
	// Zero or negative means DefaultBatchSize.
	BatchSize int

	// Interval is how long Loop waits between passes. Zero or negative
	// means DefaultInterval.
	Interval time.Duration

	// MaxAttempts is how many failed attempts an entry may record before
	// the queue reports it as exhausted. An exhausted entry stays stored
	// with its failure count and is attempted no more. Zero or negative
	// means DefaultMaxAttempts.
	MaxAttempts int

	// RetryWait is the wait Loop holds after a failed pass. Each further
	// consecutive failed pass doubles it, up to RetryMax. Zero or
	// negative means DefaultRetryWait.
	RetryWait time.Duration

	// RetryMax caps the retry wait. A cap below RetryWait reads as
	// RetryWait. Zero or negative means DefaultRetryMax.
	RetryMax time.Duration

	// Log receives one line per recorded fault, such as a pass Loop could
	// not complete. Nil means slog.Default.
	Log *slog.Logger

	// Now stamps each entry's AddedAt. Nil means time.Now.
	Now func() time.Time
}

// batch returns the batch size, substituting the default for an unset value.
func (c Config) batch() int {
	if c.BatchSize <= 0 {
		return DefaultBatchSize
	}
	return c.BatchSize
}

// interval returns the wait between passes, substituting the default for an
// unset value.
func (c Config) interval() time.Duration {
	if c.Interval <= 0 {
		return DefaultInterval
	}
	return c.Interval
}

// attempts returns the attempt cap, substituting the default for an unset
// value.
func (c Config) attempts() int {
	if c.MaxAttempts <= 0 {
		return DefaultMaxAttempts
	}
	return c.MaxAttempts
}

// wait returns the first retry wait, substituting the default for an unset
// value.
func (c Config) wait() time.Duration {
	if c.RetryWait <= 0 {
		return DefaultRetryWait
	}
	return c.RetryWait
}

// maxWait returns the retry wait cap. An unset cap means DefaultRetryMax,
// and a cap below the first wait reads as the first wait, so the curve
// starts inside its own range.
func (c Config) maxWait() time.Duration {
	limit := c.RetryMax
	if limit <= 0 {
		limit = DefaultRetryMax
	}
	if limit < c.wait() {
		return c.wait()
	}
	return limit
}

// Outbox appends events durably and delivers them to the sink in passes.
// Build one with Open, one per process. Add is safe for concurrent use.
// Flush serialises passes, so concurrent Flush calls and a Loop deliver in
// insertion order whatever order their calls arrive in.
type Outbox struct {
	store       Store
	sink        Sink
	log         *slog.Logger
	now         func() time.Time
	batchSize   int
	interval    time.Duration
	maxAttempts int
	retryWait   time.Duration
	retryMax    time.Duration

	// flushMu serialises passes, so two passes can never hand the sink
	// interleaved batches.
	flushMu sync.Mutex
}

// Open returns an Outbox that remembers entries in cfg.Store and delivers
// them to cfg.Sink. It reads one row from the store, so a schema that is
// not in place stops the open instead of the first flush. That read costs
// the same however long the queue is.
func Open(ctx context.Context, cfg Config) (*Outbox, error) {
	if cfg.Store == nil {
		return nil, ErrNoStore
	}
	if cfg.Sink == nil {
		return nil, ErrNoSink
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	o := &Outbox{
		store:       cfg.Store,
		sink:        cfg.Sink,
		log:         log,
		now:         now,
		batchSize:   cfg.batch(),
		interval:    cfg.interval(),
		maxAttempts: cfg.attempts(),
		retryWait:   cfg.wait(),
		retryMax:    cfg.maxWait(),
	}
	if _, err := o.store.Pending(ctx, math.MaxInt, 1); err != nil {
		return nil, fmt.Errorf("outbox: open: %w", err)
	}
	return o, nil
}

// Add records one event durably and returns the stored entry. The id is
// assigned here and never changes, so a sink that records the ids it served
// can drop a replayed entry. The payload is opaque bytes the outbox passes
// to the sink unchanged. A cancelled context stops the add and stores
// nothing.
func (o *Outbox) Add(ctx context.Context, payload []byte) (Entry, error) {
	entryID, err := id.New()
	if err != nil {
		return Entry{}, fmt.Errorf("outbox: assign id: %w", err)
	}
	e := Entry{ID: entryID, Payload: payload, AddedAt: o.now()}
	if err := o.store.Add(ctx, e); err != nil {
		return Entry{}, fmt.Errorf("outbox: add %s: %w", entryID, err)
	}
	return e, nil
}

// Flush runs one delivery pass and reports what it did. The pass reads the
// pending entries whose failures are below the attempt cap, hands them to
// the sink in insertion order, one batch at a time, and retires a batch
// only after Deliver returned nil for it. An entry whose failures reached
// the cap is skipped, so it cannot block the entries behind it.
//
// Deliver refusing a batch records one failure on that batch's entries and
// ends the pass, so no later batch overtakes it. Flush reports the sink's
// error. A context that ends during the pass also ends it, and that ending
// records no failure, because a cancelled pass is not a delivery verdict.
// The summary is zero whenever the pass ended in an error, and Pending
// reports what is still owed.
func (o *Outbox) Flush(ctx context.Context) (Summary, error) {
	o.flushMu.Lock()
	defer o.flushMu.Unlock()
	if err := ctx.Err(); err != nil {
		return Summary{}, err
	}
	pending, err := o.store.Pending(ctx, o.maxAttempts, math.MaxInt)
	if err != nil {
		return Summary{}, fmt.Errorf("outbox: flush: %w", err)
	}
	var sum Summary
	for start := 0; start < len(pending); {
		batch, next := o.nextBatch(pending, start)
		if len(batch) == 0 {
			break // nothing left to attempt in this snapshot
		}
		if err := o.sink.Deliver(ctx, batch); err != nil {
			if ctx.Err() != nil {
				return Summary{}, ctx.Err()
			}
			if ferr := o.store.RecordFailures(ctx, entryIDs(batch)); ferr != nil {
				o.log.Error("outbox: record failures", "count", len(batch), "error", ferr)
			}
			return Summary{}, fmt.Errorf("outbox: deliver batch: %w", err)
		}
		if err := o.store.Delivered(ctx, entryIDs(batch)); err != nil {
			return Summary{}, fmt.Errorf("outbox: retire batch: %w", err)
		}
		sum.Delivered += len(batch)
		start = next
	}
	return sum, nil
}

// nextBatch takes at most batchSize attemptable entries from pending at and
// after start, skipping entries whose failures are spent. It returns the
// batch and the index to continue from.
func (o *Outbox) nextBatch(pending []Entry, start int) ([]Entry, int) {
	batch := make([]Entry, 0, o.batchSize)
	i := start
	for ; i < len(pending) && len(batch) < o.batchSize; i++ {
		if pending[i].Failures >= o.maxAttempts {
			continue
		}
		batch = append(batch, pending[i])
	}
	return batch, i
}

// Pending returns every entry stored and not yet delivered, oldest first,
// including entries whose attempts are spent. It is the durable but
// unshipped record, and a caller that wants to know what is owed reads it.
func (o *Outbox) Pending(ctx context.Context) ([]Entry, error) {
	pending, err := o.store.Pending(ctx, math.MaxInt, math.MaxInt)
	if err != nil {
		return nil, fmt.Errorf("outbox: pending: %w", err)
	}
	return pending, nil
}

// Exhausted returns every stored entry whose recorded failures reached the
// attempt cap, oldest first. Such an entry stays stored with its failure
// count and is attempted no more. The caller decides what an exhausted
// entry needs next, which is why the queue reports the entries instead of
// dropping them.
func (o *Outbox) Exhausted(ctx context.Context) ([]Entry, error) {
	pending, err := o.store.Exhausted(ctx, o.maxAttempts)
	if err != nil {
		return nil, fmt.Errorf("outbox: exhausted: %w", err)
	}
	return pending, nil
}

// Loop runs delivery passes until ctx is done. One pass runs on every
// Interval. A failed pass holds RetryWait before the next one, and each
// further consecutive failure doubles that wait up to RetryMax, so a sink
// that is down is retried on a slowing curve instead of a spin. A wait is
// never shorter than Interval.
//
// Loop is the body of the goroutine a caller starts, and it is also that
// goroutine's named exit path. It starts no goroutine of its own and
// returns once ctx is done.
func (o *Outbox) Loop(ctx context.Context) {
	wait := o.interval
	retry := o.retryWait
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return // the caller's context is the only exit path
		case <-timer.C:
		}
		_, err := o.Flush(ctx)
		if err == nil {
			retry = o.retryWait
			timer.Reset(o.interval)
			continue
		}
		if ctx.Err() != nil {
			return // the pass died with the context, so the loop dies with it
		}
		o.log.Error("outbox: flush pass", "error", err)
		wait, retry = nextWaits(o.interval, o.retryMax, retry)
		timer.Reset(wait)
	}
}

// nextWaits returns the wait one failed pass schedules and the retry base
// the failure after it starts from. The wait is the retry base, raised to
// the interval when the base is shorter, and the base doubles up to
// retryMax.
func nextWaits(interval, retryMax, retry time.Duration) (time.Duration, time.Duration) {
	wait := retry
	if wait < interval {
		wait = interval
	}
	retry *= 2
	if retry > retryMax {
		retry = retryMax
	}
	return wait, retry
}

// entryIDs returns the id of every entry in batch, in order.
func entryIDs(batch []Entry) []string {
	ids := make([]string, len(batch))
	for i, e := range batch {
		ids[i] = e.ID
	}
	return ids
}
