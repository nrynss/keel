// Package lease runs paid sessions that last minutes rather than one call.
// A session rents one quota slot for a bounded time. Open counts the lease
// against a quota and against a budget through the paid call seam, so the
// opening estimate holds spend before the session runs. Close settles the
// reported duration through the same seam. Reconcile later applies the
// provider reported price as the truth and keeps both numbers on the record.
// A lease expires on its own. Expiry is a row comparison against the stored
// deadline, so no process must stay alive for it to hold. A dead process
// leaves its lease behind, and a later call reclaims the slot once the cap
// has passed. A caller supplied reason refuses new leases at once, which
// pairs with runtime flags without importing them.
package lease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/id"
)

// ErrQuota reports an open that found no free quota slot.
var ErrQuota = errors.New("lease: quota exhausted")

// ErrRefused reports an open that the kill switch refused. The message
// carries the reason the caller supplied.
var ErrRefused = errors.New("lease: lease refused")

// ErrUnknownLease reports an operation that names a lease the store holds
// no record for.
var ErrUnknownLease = errors.New("lease: unknown lease")

// ErrInvalid reports unusable input, such as a nil dependency or a lease
// that is already closed.
var ErrInvalid = errors.New("lease: invalid input")

// ErrNegativePrice reports a close or reconcile with a price below zero,
// because a refund is a separate charge and never a negative booking.
var ErrNegativePrice = errors.New("lease: negative price")

// State names where one lease stands.
type State string

// Lease states name where one lease stands. A lease moves from open to
// closed or expired, and a closed lease may later reconcile. Closing marks
// a lease one closer claimed while it settles, so a second closer loses the
// claim without settling. Expired is terminal like closed, because the
// session never ran past its cap.
const (
	// StateOpen marks a lease that still holds its quota slot.
	StateOpen State = "open"
	// StateClosing marks a lease one closer claimed while it settles.
	StateClosing State = "closing"
	// StateClosed marks a lease the holder closed with a reported price.
	StateClosed State = "closed"
	// StateExpired marks a lease whose cap passed before it closed.
	StateExpired State = "expired"
)

// Lease is one paid session. Open returns it, and Inspect reads it back.
// Settled carries the price Close booked. After Reconcile, Settled carries
// the provider number, while Reported keeps the holder reported one.
type Lease struct {
	// ID identifies the lease.
	ID string
	// State is where the lease stands.
	State State
	// OpenedAt is when the lease opened.
	OpenedAt time.Time
	// ExpiresAt is the instant the cap passes and the slot frees.
	ExpiresAt time.Time
	// ClosedAt is when the lease closed, or the zero time while open.
	ClosedAt time.Time
	// Estimate is the price Open reserved against the budget.
	Estimate cost.Price
	// Settled is the booked price. Close writes the reported price here,
	// and Reconcile overwrites it with the provider number.
	Settled cost.Price
	// Reported is the holder reported price Close booked. Reconcile
	// keeps it beside the provider number it overwrites Settled with.
	Reported cost.Price
	// Reconciled reports whether Reconcile has applied the provider
	// number.
	Reconciled bool
	// Kind names the billed operation on the ledger rows.
	Kind string
	// Owner names the budget owner, or empty for the shared pool.
	Owner string
}

// Quota bounds how many leases may stay open at once. The zero value is
// not usable, because it carries no limit. Quota is safe for concurrent
// use.
type Quota struct {
	mu    sync.Mutex
	limit int
	open  map[string]struct{}
}

// NewQuota returns a Quota that admits at most n open leases. It reports
// ErrInvalid when n is not positive, because a quota with no slot admits
// nothing and only misleads the caller.
func NewQuota(n int) (*Quota, error) {
	if n <= 0 {
		return nil, fmt.Errorf("lease: quota %d: %w", n, ErrInvalid)
	}
	return &Quota{limit: n, open: make(map[string]struct{})}, nil
}

// take holds one slot for id. It reports ErrQuota when no slot is free.
func (q *Quota) take(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.open) >= q.limit {
		return fmt.Errorf("lease: open %s: %w", id, ErrQuota)
	}
	q.open[id] = struct{}{}
	return nil
}

// free returns id's slot. A lease that never took one frees nothing.
func (q *Quota) free(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.open, id)
}

// count reports how many slots are held now.
func (q *Quota) count() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.open)
}

// Meter is the paid call seam an opener settles through. It matches the
// shape of the seam the cost package publishes, so the lease opens and
// closes through the real seam.
type Meter interface {
	// Call runs work as one paid call. It reserves the estimate before
	// the work runs and settles what the work reports.
	Call(ctx context.Context, estimate cost.Price, kind, ref string, work cost.Work) (cost.Usage, error)
}

// costMeterMatches pins Meter to the seam the cost package publishes,
// so a signature drift breaks the build here rather than at a call site.
var _ Meter = (*cost.Meter)(nil)

// Store keeps lease rows durably. The opener writes and reads through it,
// and expiry is judged from the stored deadlines, so no process must stay
// alive for a cap to hold. Close writes only when the row still reads open,
// so two closers of one lease never settle twice across processes.
type Store interface {
	// Create writes a fresh open lease row.
	Create(ctx context.Context, lease Lease) error
	// Get reads the lease stored under id, or an error matching
	// ErrUnknownLease.
	Get(ctx context.Context, id string) (Lease, error)
	// Update writes back the lease stored under id, or an error matching
	// ErrUnknownLease.
	Update(ctx context.Context, lease Lease) error
	// CloseIfOpen writes back lease only when the stored row still reads
	// open. It reports true when it wrote, or false with no error when the
	// row already moved on. An unknown id reports an error matching
	// ErrUnknownLease. Stores must compare while they write, so a loser
	// that read open earlier still loses.
	CloseIfOpen(ctx context.Context, lease Lease) (bool, error)
	// CloseIfClosing writes back lease only when the stored row still
	// reads closing. It reports true when it wrote, or false with no error
	// when the row already moved to closed or expired. An unknown id
	// reports an error matching ErrUnknownLease. Stores must compare while
	// they write, so a late finish never overwrites a row the sweep moved.
	CloseIfClosing(ctx context.Context, lease Lease) (bool, error)
	// ExpireIfOpenOrClosing writes back lease only when the stored row
	// still reads open or closing. It reports true when it wrote, or false
	// with no error when the row already moved to closed or expired. An
	// unknown id reports an error matching ErrUnknownLease. Stores must
	// compare while they write, so a late sweep never overwrites a close
	// that already finished.
	ExpireIfOpenOrClosing(ctx context.Context, lease Lease) (bool, error)
	// ExpiredOpen lists every row that still reads open or closing with a
	// deadline at or before now, so a call that names no id still finds
	// abandoned rows and rows a dead closer stranded. The order is row
	// order, and the cap bounds the count.
	ExpiredOpen(ctx context.Context, now time.Time, limit int) ([]Lease, error)
}

// Config configures a Manager.
type Config struct {
	// Quota bounds how many leases may stay open at once. It must not be
	// nil.
	Quota *Quota
	// Meter settles the open estimate and the close price through the
	// paid call seam. It must not be nil.
	Meter Meter
	// Store keeps the lease rows. It must not be nil.
	Store Store
	// Cap bounds how long one lease may stay open. Zero or negative
	// means defaultCap.
	Cap time.Duration
	// Kind names the billed operation on the ledger rows the seam
	// writes. Empty means defaultKind.
	Kind string
	// Now supplies the clock that stamps leases and judges expiry. Nil
	// means time.Now.
	Now func() time.Time
	// NewID mints lease ids. Nil means id.New.
	NewID func() (string, error)
	// Log receives one line per refused open and per expired lease. Nil
	// means slog.Default.
	Log *slog.Logger
}

// defaultCap bounds one lease when Config leaves Cap unset. Thirty minutes
// outlasts a normal session and frees an abandoned slot reasonably soon.
const defaultCap = 30 * time.Minute

// defaultKind names the billed operation when Config leaves Kind empty.
const defaultKind = "session"

// Manager opens, closes and reconciles paid session leases. Create it with
// New. A Manager is safe for concurrent use, because the quota, the meter
// and the store each are. Close claims the row before it settles, so two
// closers of one lease never settle twice in any process.
type Manager struct {
	quota *Quota
	meter Meter
	store Store
	cap   time.Duration
	kind  string
	now   func() time.Time
	newID func() (string, error)
	log   *slog.Logger
}

// New returns a Manager that runs leases over cfg. It reports ErrInvalid
// when the quota, the meter or the store is nil, because a lease that
// could not bound, settle or remember a session would only pretend to.
func New(cfg Config) (*Manager, error) {
	if cfg.Quota == nil {
		return nil, fmt.Errorf("lease: new: %w: nil quota", ErrInvalid)
	}
	if cfg.Meter == nil {
		return nil, fmt.Errorf("lease: new: %w: nil meter", ErrInvalid)
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("lease: new: %w: nil store", ErrInvalid)
	}
	leaseCap := cfg.Cap
	if leaseCap <= 0 {
		leaseCap = defaultCap
	}
	kind := cfg.Kind
	if kind == "" {
		kind = defaultKind
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	newID := cfg.NewID
	if newID == nil {
		newID = id.New
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		quota: cfg.Quota,
		meter: cfg.Meter,
		store: cfg.Store,
		cap:   leaseCap,
		kind:  kind,
		now:   now,
		newID: newID,
		log:   log,
	}, nil
}

// Open starts one lease for owner. A non-empty kill refuses the open at
// once with ErrRefused carrying the reason, before the lease takes a quota
// slot or touches the budget. Otherwise Open takes a quota slot, then
// settles the estimate through the seam, so a refused budget frees the
// slot it just took. The lease row lands in the store before Open returns,
// so a crash after Open still leaves a reclaimable lease.
func (m *Manager) Open(ctx context.Context, owner string, estimate cost.Price, kill string) (Lease, error) {
	if kill != "" {
		m.log.InfoContext(ctx, "lease: open refused", "reason", kill)
		return Lease{}, fmt.Errorf("lease: open refused: %s: %w", kill, ErrRefused)
	}
	if estimate < 0 {
		return Lease{}, fmt.Errorf("lease: open estimate %d: %w", estimate, ErrNegativePrice)
	}
	id, err := m.newID()
	if err != nil {
		return Lease{}, fmt.Errorf("lease: open: generate id: %w", err)
	}
	if err := m.quota.take(id); err != nil {
		return Lease{}, err
	}
	releaseSlot := true
	defer func() {
		if releaseSlot {
			m.quota.free(id)
		}
	}()
	now := m.now()
	lease := Lease{
		ID:        id,
		State:     StateOpen,
		OpenedAt:  now,
		ExpiresAt: now.Add(m.cap),
		Estimate:  estimate,
		Kind:      m.kind,
		Owner:     owner,
	}
	ref := id
	if owner != "" {
		ref = owner + "/" + id
	}
	if _, err := m.meter.Call(ctx, estimate, m.kind, ref, func(context.Context) (cost.Usage, error) {
		return cost.Usage{Price: estimate, Measured: true}, nil
	}); err != nil {
		return Lease{}, fmt.Errorf("lease: open: settle estimate: %w", err)
	}
	if err := m.store.Create(ctx, lease); err != nil {
		return Lease{}, fmt.Errorf("lease: open: %w", err)
	}
	releaseSlot = false
	return lease, nil
}

// Close ends the open lease id at the holder reported price. It claims the
// row, then settles the real duration through the seam and frees the quota
// slot. A past-cap lease expires first and frees its slot on the way out,
// then Close still reports ErrInvalid, because the session never closed in
// time. That expiry covers a closing row stranded by a dead closer too, and
// the retry reports ErrInvalid without settling, because the settle may
// already have run. One session books its close price at most once. The
// claim writes closing only when the row still reads open, so the second
// closer of one lease loses the claim and reports ErrInvalid without
// settling. The final write lands only while the row still reads closing,
// so a sweep that expired the row first makes the late close report
// ErrInvalid without touching the stored row. The claim covers every
// process, because the store compares while it writes.
func (m *Manager) Close(ctx context.Context, id string, price cost.Price) (Lease, error) {
	if price < 0 {
		return Lease{}, fmt.Errorf("lease: close price %d: %w", price, ErrNegativePrice)
	}
	lease, err := m.store.Get(ctx, id)
	if err != nil {
		return Lease{}, err
	}
	m.settleExpiry(ctx, &lease)
	if lease.State != StateOpen {
		return Lease{}, fmt.Errorf("lease: close %s in state %s: %w", id, lease.State, ErrInvalid)
	}
	ref := lease.ID
	if lease.Owner != "" {
		ref = lease.Owner + "/" + lease.ID
	}
	closed := lease
	closed.State = StateClosing
	claimed, err := m.store.CloseIfOpen(ctx, closed)
	if err != nil {
		return Lease{}, fmt.Errorf("lease: close: %w", err)
	}
	if !claimed {
		return Lease{}, fmt.Errorf("lease: close %s in state closed: %w", id, ErrInvalid)
	}
	if _, err := m.meter.Call(ctx, lease.Estimate, lease.Kind, ref, func(context.Context) (cost.Usage, error) {
		return cost.Usage{Price: price, Measured: true}, nil
	}); err != nil {
		m.releaseClaim(ctx, closed)
		return Lease{}, fmt.Errorf("lease: close: settle price: %w", err)
	}
	closed.State = StateClosed
	closed.ClosedAt = m.now()
	closed.Settled = price
	closed.Reported = price
	finished, err := m.store.CloseIfClosing(ctx, closed)
	if err != nil {
		return Lease{}, fmt.Errorf("lease: close: %w", err)
	}
	if !finished {
		stored, getErr := m.store.Get(ctx, id)
		if getErr != nil {
			return Lease{}, fmt.Errorf("lease: close: %w", getErr)
		}
		if stored.State == StateExpired {
			m.quota.free(id)
		}
		return Lease{}, fmt.Errorf("lease: close %s in state %s: %w", id, stored.State, ErrInvalid)
	}
	m.quota.free(id)
	return closed, nil
}

// releaseClaim returns a closing row to open after the settle failed, so the
// next close may try again. It writes only while the stored row still reads
// closing, so a sweep that expired the row first keeps its terminal state.
// A row the sweep already moved stays as the sweep left it, and the caller
// still reports the settle failure it holds. It logs and keeps the claim
// when the write fails, because the stored row still names the closer as
// closing.
func (m *Manager) releaseClaim(ctx context.Context, claimed Lease) {
	claimed.State = StateOpen
	if _, err := m.store.CloseIfClosing(ctx, claimed); err != nil {
		m.log.ErrorContext(ctx, "lease: release claim", "id", claimed.ID, "error", err)
	}
}

// Reconcile applies the provider reported price to the closed lease id.
// The provider number is the truth, so Settled carries it afterwards,
// while Reported keeps the holder reported price Close booked. Both stay
// observable on the returned lease and in the store. Reconcile may raise
// or lower the settled price. It writes the row only and books nothing
// further, because the budget already holds the estimate and the reported
// price as facts, while the row carries the truth. It reports ErrInvalid
// for a lease that is not closed or that already reconciled.
func (m *Manager) Reconcile(ctx context.Context, id string, provider cost.Price) (Lease, error) {
	if provider < 0 {
		return Lease{}, fmt.Errorf("lease: reconcile price %d: %w", provider, ErrNegativePrice)
	}
	lease, err := m.store.Get(ctx, id)
	if err != nil {
		return Lease{}, err
	}
	m.settleExpiry(ctx, &lease)
	if lease.State != StateClosed {
		return Lease{}, fmt.Errorf("lease: reconcile %s in state %s: %w", id, lease.State, ErrInvalid)
	}
	if lease.Reconciled {
		return Lease{}, fmt.Errorf("lease: reconcile %s twice: %w", id, ErrInvalid)
	}
	lease.Settled = provider
	lease.Reconciled = true
	if err := m.store.Update(ctx, lease); err != nil {
		return Lease{}, fmt.Errorf("lease: reconcile: %w", err)
	}
	return lease, nil
}

// Inspect reads the lease stored under id. A lease whose cap has passed
// reads as expired and frees its quota slot, even when no process touched
// it since it opened. The stored row moves to expired too, so the fact
// survives a restart.
func (m *Manager) Inspect(ctx context.Context, id string) (Lease, error) {
	lease, err := m.store.Get(ctx, id)
	if err != nil {
		return Lease{}, err
	}
	m.settleExpiry(ctx, &lease)
	return lease, nil
}

// Reclaim frees the quota slot of every lease whose cap has passed while it
// still reads open or closing. It reports how many slots it freed. An empty
// id list sweeps the store for every such row, so a lease whose id nobody
// kept still frees its row on disk. That sweep covers a closing row stranded
// by a dead closer, which expires like an open one. A named list reclaims
// exactly those ids and reports ErrUnknownLease for the first id the store
// never held, so a misspelled id never reads as success. A dead process
// leaves its lease behind, and the next call reclaims the slot without that
// process. Each reclaimed row moves to expired in the store, so the fact
// survives a restart. Reclaim books nothing, because expiry never settles
// a price.
func (m *Manager) Reclaim(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return m.sweepExpired(ctx)
	}
	freed := 0
	for _, id := range ids {
		lease, err := m.store.Get(ctx, id)
		if err != nil {
			return freed, err
		}
		if m.settleExpiry(ctx, &lease) {
			freed++
		}
	}
	return freed, nil
}

// sweepCap bounds one sweep of expired but unsettled rows. One pass frees
// the quota map without growing memory with the table.
const sweepCap = 256

// sweepExpired expires every open or stranded closing row whose deadline
// reached now, without settling any price. It reads one bounded pass and
// frees each row it finds, so the call ends even when the table holds more
// rows than the cap.
func (m *Manager) sweepExpired(ctx context.Context) (int, error) {
	rows, err := m.store.ExpiredOpen(ctx, m.now(), sweepCap)
	if err != nil {
		return 0, err
	}
	freed := 0
	for _, row := range rows {
		lease, err := m.store.Get(ctx, row.ID)
		if err != nil {
			if errors.Is(err, ErrUnknownLease) {
				continue
			}
			return freed, err
		}
		if m.settleExpiry(ctx, &lease) {
			freed++
		}
	}
	return freed, nil
}

// Active reports how many quota slots the manager holds now.
func (m *Manager) Active() int {
	return m.quota.count()
}

// settleExpiry expires lease when its cap has passed. Every touch point
// runs it first, so a past-cap lease frees its slot on the next call from
// any direction, without its holder acting. It reports whether this call
// expired the lease. A closed lease never expires, because its session ran
// its course. A closing row stranded by a dead closer expires past its cap
// like an open one, because the settle may never have run. The stranded
// spend stays exactly what the seam settled, and a retry never settles
// again, so one session books its close price at most once. The write only
// lands while the stored row still reads open or closing, so a sweep that
// loses to a finished close leaves the closed row alone.
func (m *Manager) settleExpiry(ctx context.Context, lease *Lease) bool {
	if m.now().Before(lease.ExpiresAt) {
		return false
	}
	if lease.State != StateOpen && lease.State != StateClosing {
		return false
	}
	m.expire(ctx, lease)
	return lease.State == StateExpired
}

// expire moves lease to expired in the store and frees its quota slot. The
// write only lands while the stored row still reads open or closing, so a
// sweep that loses to a finished close keeps the closed row and reports no
// expiry. A failed write still frees the slot, because the stored deadline
// judges expiry again on the next touch.
func (m *Manager) expire(ctx context.Context, lease *Lease) {
	want := *lease
	want.State = StateExpired
	moved, err := m.store.ExpireIfOpenOrClosing(ctx, want)
	if err != nil {
		m.log.ErrorContext(ctx, "lease: expire", "id", lease.ID, "error", err)
	}
	if !moved {
		stored, getErr := m.store.Get(ctx, lease.ID)
		if getErr == nil {
			*lease = stored
		}
		if getErr != nil || stored.State != StateExpired {
			return
		}
		m.quota.free(lease.ID)
		m.log.InfoContext(ctx, "lease: expired", "id", lease.ID)
		return
	}
	*lease = want
	m.quota.free(lease.ID)
	m.log.InfoContext(ctx, "lease: expired", "id", lease.ID)
}
