package sqlitestore_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/cost/sqlitestore"
)

// withPeriod sets the budget window.
func withPeriod(period cost.Period) func(*sqlitestore.Config) {
	return func(c *sqlitestore.Config) { c.Period = period }
}

// mustHold reserves estimate or ends the test.
func mustHold(t *testing.T, store *sqlitestore.Store, estimate cost.Price) sqlitestore.Reservation {
	t.Helper()
	hold, err := store.Reserve(t.Context(), estimate)
	if err != nil {
		t.Fatalf("reserve %d: %v", estimate, err)
	}
	return hold
}

// mustBook settles a hold at its actual price or ends the test.
func mustBook(t *testing.T, store *sqlitestore.Store, hold sqlitestore.Reservation, actual cost.Price) {
	t.Helper()
	if err := store.Settle(t.Context(), hold, actual); err != nil {
		t.Fatalf("settle %d: %v", actual, err)
	}
}

// mustSpent reads the booked spend or ends the test.
func mustSpent(t *testing.T, store *sqlitestore.Store) cost.Price {
	t.Helper()
	spent, err := store.Spent(t.Context())
	if err != nil {
		t.Fatalf("spent: %v", err)
	}
	return spent
}

// mustHeadroom reads the remaining headroom or ends the test.
func mustHeadroom(t *testing.T, store *sqlitestore.Store) cost.Price {
	t.Helper()
	remaining, err := store.Remaining(t.Context())
	if err != nil {
		t.Fatalf("remaining: %v", err)
	}
	return remaining
}

// TestSettleHistoryLandsBesideTheBooking reads the settle rows back over a
// fresh connection a test helper owns, so the window math below stands on a
// record the package API never touches. One settle writes exactly one row
// with the booked amount and the clock the store stamps.
func TestSettleHistoryLandsBesideTheBooking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	clock := &testClock{at: base}
	store := openStore(t, path, withLimit(1000), withClock(clock.now))
	mustBook(t, store, mustHold(t, store, 400), 300)

	var owner string
	var amount, created int64
	row := openFresh(t, path).QueryRow(`SELECT owner, amount_nd, created_at FROM cost_settle`)
	if err := row.Scan(&owner, &amount, &created); err != nil {
		t.Fatalf("scan settle history: %v", err)
	}
	if owner != "" || amount != 300 || created != base.UnixMilli() {
		t.Fatalf("settle row = (%q, %d, %d), want (\"\", 300, %d)", owner, amount, created, base.UnixMilli())
	}
	if n := freshCount(t, path, "cost_settle"); n != 1 {
		t.Fatalf("settle rows = %d, want 1", n)
	}
}

// TestDailyPeriodRestartsTheCeiling is the reported defect: a budget spent
// to its limit refuses again after midnight, because Spent, Remaining and
// Reserve read the window rather than the lifetime.
func TestDailyPeriodRestartsTheCeiling(t *testing.T) {
	clock := &testClock{at: base}
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"),
		withLimit(cost.Dollar), withClock(clock.now), withPeriod(cost.DailyUTC))
	ctx := t.Context()
	mustBook(t, store, mustHold(t, store, cost.Dollar), cost.Dollar)
	if got := mustHeadroom(t, store); got != 0 {
		t.Fatalf("remaining = %v, want 0", got)
	}
	if _, err := store.Reserve(ctx, cost.Cent); !errors.Is(err, cost.ErrOverBudget) {
		t.Fatalf("reserve past the daily ceiling error = %v, want ErrOverBudget", err)
	}

	clock.advance(13 * time.Hour) // 2024-03-02 01:00 UTC, past midnight
	if got := mustSpent(t, store); got != 0 {
		t.Fatalf("spent in the new day = %v, want 0", got)
	}
	if got := mustHeadroom(t, store); got != cost.Dollar {
		t.Fatalf("remaining in the new day = %v, want %v", got, cost.Dollar)
	}
	mustBook(t, store, mustHold(t, store, cost.Cent), cost.Cent)
	if got := mustSpent(t, store); got != cost.Cent {
		t.Fatalf("spent after booking = %v, want %v", got, cost.Cent)
	}
	if got, err := store.SpentSince(ctx, time.Time{}); err != nil || got != cost.Dollar+cost.Cent {
		t.Fatalf("spent since zero = %v, %v, want %v", got, err, cost.Dollar+cost.Cent)
	}
}

// TestDailyPeriodSurvivesAReopen replays the second half of the report: the
// store reopens two days later and the new day starts with a full ceiling.
func TestDailyPeriodSurvivesAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	clock := &testClock{at: base}
	store, db := openAt(t, path,
		withLimit(cost.Dollar), withClock(clock.now), withPeriod(cost.DailyUTC))
	mustBook(t, store, mustHold(t, store, cost.Dollar), cost.Dollar)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	clock.advance(48 * time.Hour)
	reopened := openStore(t, path,
		withLimit(cost.Dollar), withClock(clock.now), withPeriod(cost.DailyUTC))
	if got := mustHeadroom(t, reopened); got != cost.Dollar {
		t.Fatalf("remaining after reopen = %v, want %v", got, cost.Dollar)
	}
	if got, err := reopened.SpentSince(t.Context(), time.Time{}); err != nil || got != cost.Dollar {
		t.Fatalf("spent since zero = %v, %v, want %v", got, err, cost.Dollar)
	}
}

// TestLifetimePeriodNeverRestarts pins the default: a store with no period
// keeps one lifetime ceiling, so spend booked two days back still binds.
func TestLifetimePeriodNeverRestarts(t *testing.T) {
	clock := &testClock{at: base}
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"),
		withLimit(cost.Dollar), withClock(clock.now))
	ctx := t.Context()
	mustBook(t, store, mustHold(t, store, cost.Dollar), cost.Dollar)

	clock.advance(48 * time.Hour)
	if _, err := store.Reserve(ctx, cost.Cent); !errors.Is(err, cost.ErrOverBudget) {
		t.Fatalf("reserve past the lifetime ceiling error = %v, want ErrOverBudget", err)
	}
	if got := mustHeadroom(t, store); got != 0 {
		t.Fatalf("remaining = %v, want 0", got)
	}
	if got := mustSpent(t, store); got != cost.Dollar {
		t.Fatalf("spent = %v, want %v", got, cost.Dollar)
	}
}

// TestMonthlyPeriodRestartsAtMonthStart books spend mid March and shows a
// full ceiling in April.
func TestMonthlyPeriodRestartsAtMonthStart(t *testing.T) {
	march := time.Date(2024, 3, 15, 12, 0, 0, 0, time.UTC)
	clock := &testClock{at: march}
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"),
		withLimit(cost.Dollar), withClock(clock.now), withPeriod(cost.MonthlyUTC))
	mustBook(t, store, mustHold(t, store, cost.Dollar), cost.Dollar)
	if got := mustHeadroom(t, store); got != 0 {
		t.Fatalf("remaining = %v, want 0", got)
	}

	clock.advance(17 * 24 * time.Hour) // 2024-04-01 12:00 UTC
	if got := mustSpent(t, store); got != 0 {
		t.Fatalf("spent in April = %v, want 0", got)
	}
	if got := mustHeadroom(t, store); got != cost.Dollar {
		t.Fatalf("remaining in April = %v, want %v", got, cost.Dollar)
	}
}

// TestOpenRejectsUnknownPeriod pins the open refusal a caller must classify.
func TestOpenRejectsUnknownPeriod(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "cost.db"))
	_, err := sqlitestore.Open(t.Context(), sqlitestore.Config{DB: db, Limit: cost.Dollar, Period: cost.Period(7)})
	if !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Fatalf("open with period 7 error = %v, want ErrInvalid", err)
	}
}

// TestSpentSinceServesAnyWindow books twice and reads the history back from
// three instants, on a store with no period, because the history is kept
// whether or not a window reads it.
func TestSpentSinceServesAnyWindow(t *testing.T) {
	clock := &testClock{at: base}
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"),
		withLimit(1000), withClock(clock.now))
	ctx := t.Context()
	mustBook(t, store, mustHold(t, store, 300), 300)
	clock.advance(2 * time.Hour)
	mustBook(t, store, mustHold(t, store, 100), 100)

	if got, err := store.SpentSince(ctx, base); err != nil || got != 400 {
		t.Fatalf("spent since base = %v, %v, want 400", got, err)
	}
	if got, err := store.SpentSince(ctx, base.Add(time.Hour)); err != nil || got != 100 {
		t.Fatalf("spent since base plus one hour = %v, %v, want 100", got, err)
	}
	if got, err := store.SpentSince(ctx, base.Add(3*time.Hour)); err != nil || got != 0 {
		t.Fatalf("spent since the future = %v, %v, want 0", got, err)
	}
	if got := mustSpent(t, store); got != 400 {
		t.Fatalf("lifetime spent = %v, want 400", got)
	}
}

// TestHoldCountsAgainstTheWindowItWasTakenIn reserves late at night with a
// hold that outlives midnight, and shows the new day starting full while the
// late settle still books into the new day.
func TestHoldCountsAgainstTheWindowItWasTakenIn(t *testing.T) {
	evening := time.Date(2024, 3, 1, 23, 0, 0, 0, time.UTC)
	clock := &testClock{at: evening}
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"),
		withLimit(1000), withClock(clock.now), withTTL(3*time.Hour), withPeriod(cost.DailyUTC))
	hold := mustHold(t, store, 600)

	clock.advance(2 * time.Hour) // 2024-03-02 01:00 UTC, the hold is still live
	if got := mustHeadroom(t, store); got != 1000 {
		t.Fatalf("remaining in the new day = %v, want 1000", got)
	}
	if reserved, err := store.Reserved(t.Context()); err != nil || reserved != 0 {
		t.Fatalf("reserved in the new day = %v, %v, want 0", reserved, err)
	}
	mustBook(t, store, hold, 600)
	if got := mustSpent(t, store); got != 600 {
		t.Fatalf("spent after the late settle = %v, want 600", got)
	}
	if got := mustHeadroom(t, store); got != 400 {
		t.Fatalf("remaining after the late settle = %v, want 400", got)
	}
}

// TestKeyedDailyPeriodRestartsEveryOwner exhausts one owner, crosses
// midnight, and shows both owners and the global pool starting fresh, with
// the per-window global bound still refusing inside the new day.
func TestKeyedDailyPeriodRestartsEveryOwner(t *testing.T) {
	clock := &testClock{at: base}
	keyed, store := openKeyed(t, filepath.Join(t.TempDir(), "cost.db"),
		withLimit(1000), withClock(clock.now), withPeriod(cost.DailyUTC))
	mustOwnerCeiling(t, keyed, "a", 300)
	mustOwnerCeiling(t, keyed, "b", 1000)
	mustSettleKeyed(t, keyed, "a", 300)

	clock.advance(13 * time.Hour) // 2024-03-02 01:00 UTC, past midnight
	if got := mustOwnerRemaining(t, keyed, "a"); got != 300 {
		t.Fatalf("remaining for a = %v, want 300", got)
	}
	if got := mustHeadroom(t, store); got != 1000 {
		t.Fatalf("global remaining = %v, want 1000", got)
	}
	mustKeyedHold(t, keyed, "b", 700)
	holdA := mustKeyedHold(t, keyed, "a", 300)
	if _, err := keyed.Reserve(t.Context(), "b", 1); !errors.Is(err, cost.ErrOverBudget) {
		t.Fatalf("reserve past the windowed global ceiling error = %v, want ErrOverBudget", err)
	}
	if err := keyed.Release(t.Context(), "a", holdA); err != nil {
		t.Fatalf("release a: %v", err)
	}
	mustSettleKeyed(t, keyed, "b", 200)
	if got, err := keyed.SpentSince(t.Context(), "a", time.Time{}); err != nil || got != 300 {
		t.Fatalf("spent since zero for a = %v, %v, want 300", got, err)
	}
	if got, err := store.SpentSince(t.Context(), time.Time{}); err != nil || got != 500 {
		t.Fatalf("global spent since zero = %v, %v, want 500", got, err)
	}
}

// mustSettleKeyed settles an owner's hold at its actual price or ends the test.
func mustSettleKeyed(t *testing.T, keyed *sqlitestore.KeyedBudget, owner string, actual cost.Price) {
	t.Helper()
	hold, err := keyed.Reserve(t.Context(), owner, actual)
	if err != nil {
		t.Fatalf("reserve %d for %q: %v", actual, owner, err)
	}
	if err := keyed.Settle(t.Context(), owner, hold, actual); err != nil {
		t.Fatalf("settle %d for %q: %v", actual, owner, err)
	}
}

// TestKeyedSpentSinceRefusals pins the two refusals a caller must classify,
// and that a fresh owner reads zero rather than an error.
func TestKeyedSpentSinceRefusals(t *testing.T) {
	keyed, _ := openKeyed(t, filepath.Join(t.TempDir(), "cost.db"))
	if _, err := keyed.SpentSince(t.Context(), "", base); !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Fatalf("spend since with no owner error = %v, want ErrInvalid", err)
	}
	if _, err := keyed.SpentSince(t.Context(), "ghost", base); !errors.Is(err, cost.ErrUnknownOwner) {
		t.Fatalf("spend since for a ghost error = %v, want ErrUnknownOwner", err)
	}
	mustOwnerCeiling(t, keyed, "a", 1000)
	if got, err := keyed.SpentSince(t.Context(), "a", base); err != nil || got != 0 {
		t.Fatalf("spent since base for a fresh owner = %v, %v, want 0", got, err)
	}
}
