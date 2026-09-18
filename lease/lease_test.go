package lease_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/lease"
	leasesql "github.com/nrynss/keel/lease/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// base is the instant the tests stamp leases with. It carries a non-zero
// nanosecond part, so a store that lost precision would be caught.
var base = time.Date(2024, 3, 1, 12, 0, 0, 123456789, time.UTC)

// testClock is a clock a test moves by hand, so expiry needs no wall clock
// wait.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

// now returns the current instant.
func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

// advance moves the clock forward by d.
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// fakeMeter settles every call at the reported price against one budget
// and ledger, so a test observes the same balances the seam publishes.
type fakeMeter struct {
	mu     sync.Mutex
	meter  *cost.Meter
	budget *cost.Budget
	ledger *cost.Ledger
	calls  int
}

// newFakeMeter builds a meter over a fresh budget of limit.
func newFakeMeter(t *testing.T, limit cost.Price) *fakeMeter {
	t.Helper()
	budget, err := cost.NewBudget(limit)
	if err != nil {
		t.Fatalf("new budget: %v", err)
	}
	ledger := cost.NewLedger()
	meter, err := cost.NewMeter(budget, ledger)
	if err != nil {
		t.Fatalf("new meter: %v", err)
	}
	return &fakeMeter{meter: meter, budget: budget, ledger: ledger}
}

// Call runs one paid call through the real seam and counts it.
func (f *fakeMeter) Call(ctx context.Context, estimate cost.Price, kind, ref string, work cost.Work) (cost.Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.meter.Call(ctx, estimate, kind, ref, work)
}

// meterSpent reads the budget spend under the fake lock.
func (f *fakeMeter) meterSpent() cost.Price {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.budget.Spent()
}

// reserved reports the outstanding holds.
func (f *fakeMeter) reserved() cost.Price {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.budget.Reserved()
}

// remaining reports the headroom.
func (f *fakeMeter) remaining(t *testing.T) cost.Price {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	left, err := f.budget.Remaining()
	if err != nil {
		t.Fatalf("remaining: %v", err)
	}
	return left
}

// total reports the ledger total.
func (f *fakeMeter) total(t *testing.T) cost.Price {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	sum, err := f.ledger.Total()
	if err != nil {
		t.Fatalf("ledger total: %v", err)
	}
	return sum
}

// openDB opens the database file at path and closes it at cleanup.
func openDB(t *testing.T, path string) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Config{
		Path:   path,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() }) // the handle is discarded here, so a close failure cannot fail the test
	return db
}

// fixture opens one manager over a fresh database with the given quota and
// budget limits. It returns the manager, its meter, its store and the
// clock driving it.
func fixture(t *testing.T, path string, quota int, budget cost.Price, clock *testClock) (*lease.Manager, *fakeMeter, *leasesql.Store) {
	t.Helper()
	db := openDB(t, path)
	store, err := leasesql.Open(t.Context(), leasesql.Config{
		DB:     db,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("open lease store: %v", err)
	}
	meter := newFakeMeter(t, budget)
	q, err := lease.NewQuota(quota)
	if err != nil {
		t.Fatalf("new quota: %v", err)
	}
	manager, err := lease.New(lease.Config{
		Quota: q,
		Meter: meter,
		Store: store,
		Cap:   time.Hour,
		Kind:  "session",
		Now:   clock.now,
		Log:   slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	return manager, meter, store
}

// openLease opens one lease or ends the test.
func openLease(t *testing.T, m *lease.Manager, estimate cost.Price) lease.Lease {
	t.Helper()
	l, err := m.Open(t.Context(), "", estimate, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return l
}

// TestCapExpiresALeaseWithNoProcessInvolved opens one lease, moves the
// injected clock past its cap, and pins that the lease reads expired and
// its quota slot is free, with no call in between.
func TestCapExpiresALeaseWithNoProcessInvolved(t *testing.T) {
	clock := &testClock{at: base}
	manager, _, _ := fixture(t, t.TempDir()+"/lease.db", 1, 1000, clock)
	ctx := t.Context()

	got := openLease(t, manager, 100)
	if got.State != lease.StateOpen {
		t.Fatalf("open state = %q, want open", got.State)
	}
	if !got.ExpiresAt.Equal(base.Add(time.Hour)) {
		t.Fatalf("expiry = %v, want %v", got.ExpiresAt, base.Add(time.Hour))
	}
	if manager.Active() != 1 {
		t.Fatalf("active leases = %d, want 1", manager.Active())
	}

	clock.advance(61 * time.Minute)
	read, err := manager.Inspect(ctx, got.ID)
	if err != nil {
		t.Fatalf("inspect after the cap: %v", err)
	}
	if read.State != lease.StateExpired {
		t.Fatalf("state after the cap = %q, want expired", read.State)
	}
	if manager.Active() != 0 {
		t.Fatalf("active leases after expiry = %d, want 0", manager.Active())
	}

	next := openLease(t, manager, 100)
	if next.State != lease.StateOpen {
		t.Fatalf("reopen state = %q, want open", next.State)
	}
	if manager.Active() != 1 {
		t.Fatalf("active leases after reopen = %d, want 1", manager.Active())
	}
}

// TestQuotaRefusesALeasePastTheLimit fills the one quota slot and pins
// that the next open fails with ErrQuota and books nothing.
func TestQuotaRefusesALeasePastTheLimit(t *testing.T) {
	clock := &testClock{at: base}
	manager, meter, _ := fixture(t, t.TempDir()+"/lease.db", 1, 1000, clock)

	openLease(t, manager, 100)
	spent := meter.meterSpent()
	if spent != 100 {
		t.Fatalf("spent after one open = %d, want 100", spent)
	}
	if _, err := manager.Open(t.Context(), "", 100, ""); !errors.Is(err, lease.ErrQuota) {
		t.Fatalf("open past the quota error = %v, want ErrQuota", err)
	}
	if got := meter.meterSpent(); got != spent {
		t.Fatalf("spent after the refusal = %d, want %d", got, spent)
	}
	if got := meter.reserved(); got != 0 {
		t.Fatalf("reserved after the refusal = %d, want 0", got)
	}
	if manager.Active() != 1 {
		t.Fatalf("active leases = %d, want 1", manager.Active())
	}
}

// TestKillSwitchRefusesWithItsReason pins that a non-empty kill reason
// refuses the open with ErrRefused carrying the reason, and that the
// refusal takes no quota slot and books nothing.
func TestKillSwitchRefusesWithItsReason(t *testing.T) {
	clock := &testClock{at: base}
	manager, meter, _ := fixture(t, t.TempDir()+"/lease.db", 1, 1000, clock)

	_, err := manager.Open(t.Context(), "", 100, "provider maintenance")
	if !errors.Is(err, lease.ErrRefused) {
		t.Fatalf("kill switch error = %v, want ErrRefused", err)
	}
	text := fmt.Sprint(err)
	if text == "" {
		t.Fatalf("kill switch error is empty, want the reason")
	}
	if !contains(text, "provider maintenance") {
		t.Fatalf("kill switch error = %q, want the reason inside", text)
	}
	if manager.Active() != 0 {
		t.Fatalf("active leases after the refusal = %d, want 0", manager.Active())
	}
	if got := meter.meterSpent(); got != 0 {
		t.Fatalf("spent after the refusal = %d, want 0", got)
	}

	got := openLease(t, manager, 100)
	if got.State != lease.StateOpen {
		t.Fatalf("open after the switch cleared = %q, want open", got.State)
	}
}

// contains reports whether s holds part.
func contains(s, part string) bool {
	for i := 0; i+len(part) <= len(s); i++ {
		if s[i:i+len(part)] == part {
			return true
		}
	}
	return false
}

// TestReconcileUpAndDown pins both directions. The provider number becomes
// the settled truth while the holder reported number stays recorded beside
// it, and the budget and ledger move by the exact delta each way.
func TestReconcileUpAndDown(t *testing.T) {
	clock := &testClock{at: base}
	manager, meter, store := fixture(t, t.TempDir()+"/lease.db", 2, 1000, clock)
	ctx := t.Context()

	up := openLease(t, manager, 100)
	closed, err := manager.Close(ctx, up.ID, 120)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if closed.Settled != 120 || closed.Reported != 120 {
		t.Fatalf("closed settled = %d at %d, want 120 at 120", closed.Settled, closed.Reported)
	}
	afterUp, err := manager.Reconcile(ctx, up.ID, 150)
	if err != nil {
		t.Fatalf("reconcile up: %v", err)
	}
	if afterUp.Settled != 150 {
		t.Fatalf("reconciled settled = %d, want the provider 150", afterUp.Settled)
	}
	if afterUp.Reported != 120 {
		t.Fatalf("reconciled reported = %d, want the original 120", afterUp.Reported)
	}
	if !afterUp.Reconciled {
		t.Fatalf("reconciled flag is false, want true")
	}

	down := openLease(t, manager, 100)
	if _, err := manager.Close(ctx, down.ID, 120); err != nil {
		t.Fatalf("close: %v", err)
	}
	afterDown, err := manager.Reconcile(ctx, down.ID, 80)
	if err != nil {
		t.Fatalf("reconcile down: %v", err)
	}
	if afterDown.Settled != 80 {
		t.Fatalf("reconciled settled = %d, want the provider 80", afterDown.Settled)
	}
	if afterDown.Reported != 120 {
		t.Fatalf("reconciled reported = %d, want the original 120", afterDown.Reported)
	}

	stored, err := store.Get(ctx, up.ID)
	if err != nil {
		t.Fatalf("read stored lease: %v", err)
	}
	if stored.Settled != 150 || stored.Reported != 120 || !stored.Reconciled {
		t.Fatalf("stored lease = %+v, want settled 150 at 120 reconciled", stored)
	}
	if got := meter.meterSpent(); got != 100+120+100+120 {
		t.Fatalf("spent = %d, want %d", got, 100+120+100+120)
	}
	if got := meter.total(t); got != 100+120+100+120 {
		t.Fatalf("ledger total = %d, want %d", got, 100+120+100+120)
	}
}

// TestAbandonedLeaseReclaimedAfterItsCap opens one lease, moves the clock
// past its cap with no close, and pins that reclaim frees the slot so a
// new lease opens.
func TestAbandonedLeaseReclaimedAfterItsCap(t *testing.T) {
	clock := &testClock{at: base}
	manager, _, _ := fixture(t, t.TempDir()+"/lease.db", 1, 1000, clock)
	ctx := t.Context()

	abandoned := openLease(t, manager, 100)
	clock.advance(61 * time.Minute)
	freed, err := manager.Reclaim(ctx, []string{abandoned.ID})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if freed != 1 {
		t.Fatalf("reclaimed = %d, want 1", freed)
	}
	if manager.Active() != 0 {
		t.Fatalf("active leases after reclaim = %d, want 0", manager.Active())
	}
	read, err := manager.Inspect(ctx, abandoned.ID)
	if err != nil {
		t.Fatalf("inspect the reclaimed lease: %v", err)
	}
	if read.State != lease.StateExpired {
		t.Fatalf("reclaimed state = %q, want expired", read.State)
	}
	next := openLease(t, manager, 100)
	if next.State != lease.StateOpen {
		t.Fatalf("open after reclaim = %q, want open", next.State)
	}
}

// TestSeveralOpenersAgainstOneQuota runs concurrent openers past one quota
// and pins the exact final counts: the quota grants exactly its slots and
// the budget books exactly those opens.
func TestSeveralOpenersAgainstOneQuota(t *testing.T) {
	clock := &testClock{at: base}
	manager, meter, _ := fixture(t, t.TempDir()+"/lease.db", 8, 800, clock)

	const attempts = 64
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := manager.Open(context.Background(), "", 100, "")
			switch {
			case err == nil:
				mu.Lock()
				granted++
				mu.Unlock()
			case !errors.Is(err, lease.ErrQuota):
				t.Errorf("open error = %v, want nil or ErrQuota", err)
			}
		}()
	}
	wg.Wait()
	if granted != 8 {
		t.Fatalf("granted %d leases, want the full quota 8", granted)
	}
	if got := manager.Active(); got != 8 {
		t.Fatalf("active leases = %d, want 8", got)
	}
	if got := meter.meterSpent(); got != 800 {
		t.Fatalf("spent = %d, want 800", got)
	}
	if got := meter.reserved(); got != 0 {
		t.Fatalf("reserved = %d, want 0", got)
	}
	if got := meter.remaining(t); got != 0 {
		t.Fatalf("remaining = %d, want 0", got)
	}
}

// TestCloseSettlesTheRealDuration pins that closing settles the reported
// price through the seam, so the budget books the open estimate plus the
// close price exactly.
func TestCloseSettlesTheRealDuration(t *testing.T) {
	clock := &testClock{at: base}
	manager, meter, _ := fixture(t, t.TempDir()+"/lease.db", 2, 1000, clock)
	ctx := t.Context()

	got := openLease(t, manager, 100)
	clock.advance(10 * time.Minute)
	closed, err := manager.Close(ctx, got.ID, 230)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if closed.State != lease.StateClosed {
		t.Fatalf("closed state = %q, want closed", closed.State)
	}
	if closed.Settled != 230 || closed.Reported != 230 {
		t.Fatalf("closed settled = %d at %d, want 230 at 230", closed.Settled, closed.Reported)
	}
	if manager.Active() != 0 {
		t.Fatalf("active leases after close = %d, want 0", manager.Active())
	}
	if got := meter.meterSpent(); got != 330 {
		t.Fatalf("spent = %d, want 330", got)
	}
}

// TestManagerRefusesBadInput pins the constructor and the price guards.
func TestManagerRefusesBadInput(t *testing.T) {
	clock := &testClock{at: base}
	manager, _, _ := fixture(t, t.TempDir()+"/lease.db", 1, 1000, clock)
	if _, err := lease.New(lease.Config{}); !errors.Is(err, lease.ErrInvalid) {
		t.Fatalf("new without dependencies error = %v, want ErrInvalid", err)
	}
	if _, err := lease.NewQuota(0); !errors.Is(err, lease.ErrInvalid) {
		t.Fatalf("quota of zero error = %v, want ErrInvalid", err)
	}
	if _, err := manager.Open(t.Context(), "", -1, ""); !errors.Is(err, lease.ErrNegativePrice) {
		t.Fatalf("open with a negative estimate error = %v, want ErrNegativePrice", err)
	}
	got := openLease(t, manager, 100)
	if _, err := manager.Close(t.Context(), got.ID, -1); !errors.Is(err, lease.ErrNegativePrice) {
		t.Fatalf("close with a negative price error = %v, want ErrNegativePrice", err)
	}
	if _, err := manager.Close(t.Context(), "missing", 10); !errors.Is(err, lease.ErrUnknownLease) {
		t.Fatalf("close of a missing lease error = %v, want ErrUnknownLease", err)
	}
	if _, err := manager.Close(t.Context(), got.ID, 10); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := manager.Close(t.Context(), got.ID, 10); !errors.Is(err, lease.ErrInvalid) {
		t.Fatalf("second close error = %v, want ErrInvalid", err)
	}
	if _, err := manager.Reconcile(t.Context(), got.ID, -1); !errors.Is(err, lease.ErrNegativePrice) {
		t.Fatalf("reconcile with a negative price error = %v, want ErrNegativePrice", err)
	}
}

// TestExpirySurvivesARestart reopens the same file with a clock past the
// cap and pins that the lease reads expired with no process carrying it.
// The reopened manager judges the stored deadline with its own clock, so
// the fact holds across the restart.
func TestExpirySurvivesARestart(t *testing.T) {
	path := t.TempDir() + "/lease.db"
	clock := &testClock{at: base}
	manager, _, _ := fixture(t, path, 1, 1000, clock)
	got := openLease(t, manager, 100)

	clock.advance(61 * time.Minute)
	db := openDB(t, path)
	store, err := leasesql.Open(t.Context(), leasesql.Config{
		DB:     db,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("reopen lease store: %v", err)
	}
	quota, err := lease.NewQuota(1)
	if err != nil {
		t.Fatalf("new quota: %v", err)
	}
	restarted, err := lease.New(lease.Config{
		Quota: quota,
		Meter: newFakeMeter(t, 1000),
		Store: store,
		Cap:   time.Hour,
		Kind:  "session",
		Now:   clock.now,
		Log:   slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("new restarted manager: %v", err)
	}
	read, err := restarted.Inspect(t.Context(), got.ID)
	if err != nil {
		t.Fatalf("read after restart: %v", err)
	}
	if read.State != lease.StateExpired {
		t.Fatalf("state after restart = %q, want expired", read.State)
	}
	if restarted.Active() != 0 {
		t.Fatalf("active leases after restart = %d, want 0", restarted.Active())
	}
	// The stored row moved to expired too, so a raw read sees the fact.
	stored, err := store.Get(t.Context(), got.ID)
	if err != nil {
		t.Fatalf("read the stored row: %v", err)
	}
	if stored.State != lease.StateExpired {
		t.Fatalf("stored state after restart = %q, want expired", stored.State)
	}
}

// TestAbandonedLeaseFreesItsSlotAcrossHandles opens one lease, drops every
// handle to it without closing, then shows the slot reclaimable after the
// cap through a second manager over the same rows. No handle of the first
// manager acts again, so the second manager frees the slot on its own.
func TestAbandonedLeaseFreesItsSlotAcrossHandles(t *testing.T) {
	path := t.TempDir() + "/lease.db"
	clock := &testClock{at: base}
	first, _, _ := fixture(t, path, 1, 1000, clock)
	abandoned := openLease(t, first, 100)

	db := openDB(t, path)
	secondStore, err := leasesql.Open(t.Context(), leasesql.Config{
		DB:     db,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("reopen lease store: %v", err)
	}
	secondQuota, err := lease.NewQuota(1)
	if err != nil {
		t.Fatalf("new quota: %v", err)
	}
	secondMeter := newFakeMeter(t, 1000)
	second, err := lease.New(lease.Config{
		Quota: secondQuota,
		Meter: secondMeter,
		Store: secondStore,
		Cap:   time.Hour,
		Kind:  "session",
		Now:   clock.now,
		Log:   slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("new second manager: %v", err)
	}
	clock.advance(61 * time.Minute)
	freed, err := second.Reclaim(t.Context(), []string{abandoned.ID})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if freed != 1 {
		t.Fatalf("reclaimed = %d, want 1", freed)
	}
	read, err := second.Inspect(t.Context(), abandoned.ID)
	if err != nil {
		t.Fatalf("inspect the abandoned lease: %v", err)
	}
	if read.State != lease.StateExpired {
		t.Fatalf("abandoned state = %q, want expired", read.State)
	}
	if got := openLease(t, second, 100); got.State != lease.StateOpen {
		t.Fatalf("open after reclaim = %q, want open", got.State)
	}
}
