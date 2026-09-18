package sqlitestore_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/cost/sqlitestore"
	"github.com/nrynss/keel/id"
	"github.com/nrynss/keel/sqlite"
)

// base is the instant the tests stamp charges and holds with. It carries a
// non-zero nanosecond part so a database that stored whole seconds would be
// caught.
var base = time.Date(2024, 3, 1, 12, 0, 0, 123456789, time.UTC)

// helperEnv marks the child process the crash test spawns. The child reserves,
// then exits without releasing, so the reservation outlives the process.
const helperEnv = "KEEL_COST_CRASH_HELPER"

// testClock is a clock a test moves by hand, so expiry needs no wall clock wait.
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

// withLimit sets the budget ceiling.
func withLimit(limit cost.Price) func(*sqlitestore.Config) {
	return func(c *sqlitestore.Config) { c.Limit = limit }
}

// withClock sets the injected clock.
func withClock(now func() time.Time) func(*sqlitestore.Config) {
	return func(c *sqlitestore.Config) { c.Now = now }
}

// withTTL sets the reservation time to live.
func withTTL(d time.Duration) func(*sqlitestore.Config) {
	return func(c *sqlitestore.Config) { c.ReservationTTL = d }
}

// openDB opens a SQLite database at path and closes it at cleanup.
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

// openAt opens a cost store over the file at path and returns it with the
// database handle, so a test can close the handle and reopen the same file.
func openAt(t *testing.T, path string, opts ...func(*sqlitestore.Config)) (*sqlitestore.Store, *sqlite.DB) {
	t.Helper()
	db := openDB(t, path)
	cfg := sqlitestore.Config{
		DB:     db,
		Limit:  cost.Dollar,
		Logger: slog.New(slog.DiscardHandler),
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	store, err := sqlitestore.Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("open cost store: %v", err)
	}
	return store, db
}

// openStore opens a cost store over the file at path.
func openStore(t *testing.T, path string, opts ...func(*sqlitestore.Config)) *sqlitestore.Store {
	t.Helper()
	store, _ := openAt(t, path, opts...)
	return store
}

// openFresh opens the database file at path over a new connection with no
// pragmas from any package. It is the independent observer the tests query.
func openFresh(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() }) // the handle is discarded here, so a close failure cannot fail the test
	return db
}

// freshCount counts the rows in table over a fresh connection.
func freshCount(t *testing.T, path, table string) int {
	t.Helper()
	var n int
	if err := openFresh(t, path).QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestChargesRoundTripAndTotal pins that every field survives the database and
// that the totals agree with the charges that were written.
func TestChargesRoundTripAndTotal(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"))
	ctx := t.Context()
	charges := []cost.Charge{
		{Kind: "gemini", Units: 1200, UnitPrice: cost.Microdollar, Ref: "job-1"},
		{Kind: "gemini", Units: 800, UnitPrice: cost.Microdollar, Ref: "job-2"},
		{Kind: "assemblyai", Units: 30, UnitPrice: cost.Cent, Ref: "job-1"},
	}
	var wantTotal cost.Price
	var wantGemini cost.Price
	var wantJob1 cost.Price
	for _, c := range charges {
		if err := store.Add(ctx, c); err != nil {
			t.Fatalf("add %+v: %v", c, err)
		}
		amount, err := c.Total()
		if err != nil {
			t.Fatalf("charge total: %v", err)
		}
		wantTotal += amount
		if c.Kind == "gemini" {
			wantGemini += amount
		}
		if c.Ref == "job-1" {
			wantJob1 += amount
		}
	}

	got, err := store.Charges(ctx)
	if err != nil {
		t.Fatalf("charges: %v", err)
	}
	if len(got) != len(charges) {
		t.Fatalf("got %d charges, want %d", len(got), len(charges))
	}
	for i := range charges {
		if got[i] != charges[i] {
			t.Fatalf("charge %d = %+v, want %+v", i, got[i], charges[i])
		}
	}
	total, err := store.Total(ctx)
	if err != nil {
		t.Fatalf("total: %v", err)
	}
	if total != wantTotal {
		t.Fatalf("total = %d, want %d", total, wantTotal)
	}
	byKind, err := store.TotalByKind(ctx, "gemini")
	if err != nil {
		t.Fatalf("total by kind: %v", err)
	}
	if byKind != wantGemini {
		t.Fatalf("total by kind = %d, want %d", byKind, wantGemini)
	}
	byRef, err := store.TotalForRef(ctx, "job-1")
	if err != nil {
		t.Fatalf("total for ref: %v", err)
	}
	if byRef != wantJob1 {
		t.Fatalf("total for ref = %d, want %d", byRef, wantJob1)
	}
}

// TestTotalsSurviveRestartWithAFreshConnection writes charges, closes the
// handle, reopens the file, and reads the total back over a fresh connection
// that never went through this package.
func TestTotalsSurviveRestartWithAFreshConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	store, db := openAt(t, path)
	ctx := t.Context()
	charges := []cost.Charge{
		{Kind: "gemini", Units: 1200, UnitPrice: cost.Microdollar, Ref: "job-1"},
		{Kind: "assemblyai", Units: 30, UnitPrice: cost.Cent, Ref: "job-1"},
	}
	var want cost.Price
	for _, c := range charges {
		if err := store.Add(ctx, c); err != nil {
			t.Fatalf("add: %v", err)
		}
		amount, err := c.Total()
		if err != nil {
			t.Fatalf("charge total: %v", err)
		}
		want += amount
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close handle: %v", err)
	}

	reopened := openStore(t, path)
	got, err := reopened.Total(ctx)
	if err != nil {
		t.Fatalf("total after restart: %v", err)
	}
	if got != want {
		t.Fatalf("total after restart = %d, want %d", got, want)
	}

	var raw int64
	if err := openFresh(t, path).
		QueryRow(`SELECT COALESCE(SUM(units * unit_price), 0) FROM cost_charge`).Scan(&raw); err != nil {
		t.Fatalf("fresh sum: %v", err)
	}
	if cost.Price(raw) != want {
		t.Fatalf("fresh connection sum = %d, want %d", raw, want)
	}
}

// TestTotalReportsOverflow pins that one charge whose own product leaves the
// int64 range makes every total report the cost package's overflow sentinel.
func TestTotalReportsOverflow(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"))
	ctx := t.Context()
	if err := store.Add(ctx, cost.Charge{Kind: "big", Units: 2, UnitPrice: cost.Price(math.MaxInt64)}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := store.Total(ctx); !errors.Is(err, cost.ErrOverflow) {
		t.Fatalf("total error = %v, want ErrOverflow", err)
	}
	if _, err := store.TotalByKind(ctx, "big"); !errors.Is(err, cost.ErrOverflow) {
		t.Fatalf("total by kind error = %v, want ErrOverflow", err)
	}
	if _, err := store.TotalForRef(ctx, ""); !errors.Is(err, cost.ErrOverflow) {
		t.Fatalf("total for ref error = %v, want ErrOverflow", err)
	}
}

// TestOpenAppliesMigrationsOnce reopens the same file and counts the migration
// ledger, so a second open cannot run a schema file again. The ledger holds
// one row per schema file the package owns, and both files are applied.
func TestOpenAppliesMigrationsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	openStore(t, path)
	openStore(t, path)
	if n := freshCount(t, path, "cost_schema_migrations"); n != 2 {
		t.Fatalf("migration ledger rows = %d, want 2", n)
	}
}

// TestOpenRejectsNilDatabase and TestOpenRejectsNegativeLimit pin the two open
// refusals a caller must classify.
func TestOpenRejectsNilDatabase(t *testing.T) {
	if _, err := sqlitestore.Open(t.Context(), sqlitestore.Config{}); !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Fatalf("open with no database error = %v, want ErrInvalid", err)
	}
}

func TestOpenRejectsNegativeLimit(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "cost.db"))
	_, err := sqlitestore.Open(t.Context(), sqlitestore.Config{DB: db, Limit: -1})
	if !errors.Is(err, cost.ErrNegativeLimit) {
		t.Fatalf("open with negative limit error = %v, want ErrNegativeLimit", err)
	}
}

// TestReserveRemainingAndOverBudget pins the headroom arithmetic and the
// refusal that commits nothing.
func TestReserveRemainingAndOverBudget(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(1000))
	ctx := t.Context()
	if _, err := store.Reserve(ctx, 300); err != nil {
		t.Fatalf("reserve 300: %v", err)
	}
	if remaining, _ := store.Remaining(ctx); remaining != 700 {
		t.Fatalf("remaining = %d, want 700", remaining)
	}
	if reserved, _ := store.Reserved(ctx); reserved != 300 {
		t.Fatalf("reserved = %d, want 300", reserved)
	}
	if spent, _ := store.Spent(ctx); spent != 0 {
		t.Fatalf("spent = %d, want 0", spent)
	}
	if _, err := store.Reserve(ctx, 800); !errors.Is(err, cost.ErrOverBudget) {
		t.Fatalf("reserve 800 error = %v, want ErrOverBudget", err)
	}
	if remaining, _ := store.Remaining(ctx); remaining != 700 {
		t.Fatalf("remaining after refusal = %d, want 700", remaining)
	}
	if _, err := store.Reserve(ctx, 700); err != nil {
		t.Fatalf("reserve exactly remaining: %v", err)
	}
	if remaining, _ := store.Remaining(ctx); remaining != 0 {
		t.Fatalf("remaining = %d, want 0", remaining)
	}
	if _, err := store.Reserve(ctx, 1); !errors.Is(err, cost.ErrOverBudget) {
		t.Fatalf("reserve past the ceiling error = %v, want ErrOverBudget", err)
	}
}

// TestLimitReturnsTheCeiling pins that the stored ceiling is what Open wrote.
func TestLimitReturnsTheCeiling(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(1000))
	if limit, _ := store.Limit(t.Context()); limit != 1000 {
		t.Fatalf("limit = %d, want 1000", limit)
	}
}

// TestReserveRejectsNegativeEstimate pins the negative estimate refusal.
func TestReserveRejectsNegativeEstimate(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(1000))
	if _, err := store.Reserve(t.Context(), -1); !errors.Is(err, cost.ErrNegativeEstimate) {
		t.Fatalf("reserve -1 error = %v, want ErrNegativeEstimate", err)
	}
}

// TestSettleBooksSpendAndFreesHold pins that a settle books the actual price
// and drops the hold.
func TestSettleBooksSpendAndFreesHold(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(1000))
	ctx := t.Context()
	reservation, err := store.Reserve(ctx, 400)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := store.Settle(ctx, reservation, 250); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if spent, _ := store.Spent(ctx); spent != 250 {
		t.Fatalf("spent = %d, want 250", spent)
	}
	if reserved, _ := store.Reserved(ctx); reserved != 0 {
		t.Fatalf("reserved = %d, want 0", reserved)
	}
	if remaining, _ := store.Remaining(ctx); remaining != 750 {
		t.Fatalf("remaining = %d, want 750", remaining)
	}
}

// TestSettleReportsOverflowAndCommitsNothing pins that a booking past the
// int64 range reports the cost sentinel and leaves the stored spend alone.
func TestSettleReportsOverflowAndCommitsNothing(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(cost.Dollar))
	ctx := t.Context()
	reservation, err := store.Reserve(ctx, 1)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := store.Settle(ctx, reservation, cost.Price(math.MaxInt64)); err != nil {
		t.Fatalf("settle the ceiling: %v", err)
	}
	if spent, _ := store.Spent(ctx); spent != cost.Price(math.MaxInt64) {
		t.Fatalf("spent = %d, want MaxInt64", spent)
	}
	if err := store.Settle(ctx, sqlitestore.Reservation{}, 1); !errors.Is(err, cost.ErrOverflow) {
		t.Fatalf("settle past the range error = %v, want ErrOverflow", err)
	}
	if spent, _ := store.Spent(ctx); spent != cost.Price(math.MaxInt64) {
		t.Fatalf("spent after the refused settle = %d, want MaxInt64", spent)
	}
}

// TestReleaseFreesHold pins that a released hold returns its amount.
func TestReleaseFreesHold(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(1000))
	ctx := t.Context()
	reservation, err := store.Reserve(ctx, 400)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := store.Release(ctx, reservation); err != nil {
		t.Fatalf("release: %v", err)
	}
	if reserved, _ := store.Reserved(ctx); reserved != 0 {
		t.Fatalf("reserved = %d, want 0", reserved)
	}
	if remaining, _ := store.Remaining(ctx); remaining != 1000 {
		t.Fatalf("remaining = %d, want 1000", remaining)
	}
	// A zero reservation names no hold, so releasing it is a no-op and not an
	// error.
	if err := store.Release(ctx, sqlitestore.Reservation{}); err != nil {
		t.Fatalf("release zero reservation: %v", err)
	}
}

// TestReservationIDIsUnguessable pins that Reserve mints a real id.
func TestReservationIDIsUnguessable(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(1000))
	reservation, err := store.Reserve(t.Context(), 100)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if !id.Valid(reservation.ID) {
		t.Fatalf("reservation id %q is not a valid id", reservation.ID)
	}
	if reservation.Amount != 100 {
		t.Fatalf("reservation amount = %d, want 100", reservation.Amount)
	}
}

// TestExpiredReservationReturnsHeadroom advances the injected clock past the
// TTL and shows the headroom back at its full value and the amount reservable
// again.
func TestExpiredReservationReturnsHeadroom(t *testing.T) {
	clock := &testClock{at: base}
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"),
		withLimit(1000), withTTL(time.Minute), withClock(clock.now))
	ctx := t.Context()
	reservation, err := store.Reserve(ctx, 400)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if !reservation.ExpiresAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("expiry = %v, want %v", reservation.ExpiresAt, base.Add(time.Minute))
	}
	if remaining, _ := store.Remaining(ctx); remaining != 600 {
		t.Fatalf("remaining while held = %d, want 600", remaining)
	}

	clock.advance(61 * time.Second)
	if remaining, _ := store.Remaining(ctx); remaining != 1000 {
		t.Fatalf("remaining after expiry = %d, want 1000", remaining)
	}
	if reserved, _ := store.Reserved(ctx); reserved != 0 {
		t.Fatalf("reserved after expiry = %d, want 0", reserved)
	}
	if _, err := store.Reserve(ctx, 1000); err != nil {
		t.Fatalf("reserve the full ceiling after expiry: %v", err)
	}
}

// TestReservePurgesExpiredRows pins that a reserve clears the expired hold it
// walked past.
func TestReservePurgesExpiredRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	clock := &testClock{at: base}
	store := openStore(t, path, withLimit(1000), withTTL(time.Minute), withClock(clock.now))
	ctx := t.Context()
	if _, err := store.Reserve(ctx, 400); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	clock.advance(2 * time.Minute)
	if _, err := store.Reserve(ctx, 100); err != nil {
		t.Fatalf("reserve after expiry: %v", err)
	}
	if n := freshCount(t, path, "cost_reservation"); n != 1 {
		t.Fatalf("reservation rows = %d, want 1", n)
	}
}

// TestOpenPurgesExpiredAcrossRestart reopens the file with a clock past the
// expiry and shows the hold gone from the table and from the headroom.
func TestOpenPurgesExpiredAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	clock := &testClock{at: base}
	store := openStore(t, path, withLimit(1000), withTTL(time.Minute), withClock(clock.now))
	if _, err := store.Reserve(t.Context(), 400); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	clock.advance(2 * time.Minute)

	reopened := openStore(t, path,
		withLimit(1000), withTTL(time.Minute), withClock(clock.now))
	if reserved, _ := reopened.Reserved(t.Context()); reserved != 0 {
		t.Fatalf("reserved after restart = %d, want 0", reserved)
	}
	if remaining, _ := reopened.Remaining(t.Context()); remaining != 1000 {
		t.Fatalf("remaining after restart = %d, want 1000", remaining)
	}
	if n := freshCount(t, path, "cost_reservation"); n != 0 {
		t.Fatalf("reservation rows = %d, want 0", n)
	}
}

// TestSettleAfterExpiryStillBooksSpend pins that a call that finished after its
// hold expired still books what it cost.
func TestSettleAfterExpiryStillBooksSpend(t *testing.T) {
	clock := &testClock{at: base}
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"),
		withLimit(1000), withTTL(time.Minute), withClock(clock.now))
	ctx := t.Context()
	reservation, err := store.Reserve(ctx, 400)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	clock.advance(2 * time.Minute)
	if err := store.Settle(ctx, reservation, 250); err != nil {
		t.Fatalf("settle after expiry: %v", err)
	}
	if spent, _ := store.Spent(ctx); spent != 250 {
		t.Fatalf("spent = %d, want 250", spent)
	}
	if reserved, _ := store.Reserved(ctx); reserved != 0 {
		t.Fatalf("reserved = %d, want 0", reserved)
	}
	if remaining, _ := store.Remaining(ctx); remaining != 750 {
		t.Fatalf("remaining = %d, want 750", remaining)
	}
}

// TestConcurrentReservesNeverOverspend drives many goroutines at one durable
// budget and pins that exactly the headroom is granted.
func TestConcurrentReservesNeverOverspend(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(500))
	ctx := t.Context()
	const attempts = 1000
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Reserve(ctx, 1)
			if err == nil {
				mu.Lock()
				granted++
				mu.Unlock()
				return
			}
			if !errors.Is(err, cost.ErrOverBudget) {
				t.Errorf("reserve error = %v, want ErrOverBudget", err)
			}
		}()
	}
	wg.Wait()
	if granted != 500 {
		t.Fatalf("granted %d reservations, want 500", granted)
	}
	if reserved, _ := store.Reserved(ctx); reserved != 500 {
		t.Fatalf("reserved = %d, want 500", reserved)
	}
	if remaining, _ := store.Remaining(ctx); remaining != 0 {
		t.Fatalf("remaining = %d, want 0", remaining)
	}
}

// TestReservationCrashHelper is the child process the crash test spawns. It
// reserves, prints, and exits without releasing, so the hold must expire on its
// own. It skips when the parent did not set the marker.
func TestReservationCrashHelper(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip("helper process for the crash test")
	}
	path := os.Getenv("KEEL_COST_DB")
	limit, err := strconv.ParseInt(os.Getenv("KEEL_COST_LIMIT"), 10, 64)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad limit:", err)
		os.Exit(3)
	}
	amount, err := strconv.ParseInt(os.Getenv("KEEL_COST_AMOUNT"), 10, 64)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad amount:", err)
		os.Exit(3)
	}
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Config{Path: path, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open db:", err)
		os.Exit(4)
	}
	store, err := sqlitestore.Open(ctx, sqlitestore.Config{
		DB:     db,
		Limit:  cost.Price(limit),
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open store:", err)
		os.Exit(5)
	}
	if _, err := store.Reserve(ctx, cost.Price(amount)); err != nil {
		fmt.Fprintln(os.Stderr, "reserve:", err)
		os.Exit(6)
	}
	fmt.Println("reserved")
	// Exit without closing the database or releasing the hold. The reservation
	// survives, and only its expiry may free the budget it holds.
	os.Exit(0)
}

// TestReservationSurvivesACrashedProcess starts the helper in its own process,
// lets it reserve and die, then shows the hold present and expiring. The helper
// process is the honest part: a real process committed a real reservation and
// never released it. The clock that judges the expiry is injected, so the test
// needs no wall clock wait.
func TestReservationSurvivesACrashedProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	const limit = cost.Price(1000)
	const amount = cost.Price(400)

	cmd := exec.Command(os.Args[0], "-test.run=TestReservationCrashHelper")
	cmd.Env = append(os.Environ(),
		helperEnv+"=1",
		"KEEL_COST_DB="+path,
		"KEEL_COST_LIMIT="+strconv.FormatInt(int64(limit), 10),
		"KEEL_COST_AMOUNT="+strconv.FormatInt(int64(amount), 10),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper process: %v\n%s", err, out)
	}

	if n := freshCount(t, path, "cost_reservation"); n != 1 {
		t.Fatalf("reservation rows after the crash = %d, want 1", n)
	}
	held := openStore(t, path, withLimit(limit))
	if remaining, _ := held.Remaining(t.Context()); remaining != limit-amount {
		t.Fatalf("remaining while the dead process holds = %d, want %d", remaining, limit-amount)
	}

	future := time.Now().Add(30 * time.Minute)
	after := openStore(t, path, withLimit(limit), withClock(func() time.Time { return future }))
	if remaining, _ := after.Remaining(t.Context()); remaining != limit {
		t.Fatalf("remaining after the hold expired = %d, want %d", remaining, limit)
	}
	if _, err := after.Reserve(t.Context(), limit); err != nil {
		t.Fatalf("reserve the full ceiling after the hold expired: %v", err)
	}
}

// TestZeroConfigUsesSensibleDefaults pins that a store built with no clock and
// no TTL still holds a reservation for the default ten minutes.
func TestZeroConfigUsesSensibleDefaults(t *testing.T) {
	store, _ := openAt(t, filepath.Join(t.TempDir(), "cost.db"))
	reservation, err := store.Reserve(t.Context(), 100)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	delta := time.Until(reservation.ExpiresAt)
	if delta < 9*time.Minute || delta > 10*time.Minute {
		t.Fatalf("default expiry is %v away, want about ten minutes", delta)
	}
}

// cancelledContext returns a context a caller has already cancelled.
func cancelledContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx
}

// openDBBusy opens a SQLite database whose statements wait at most busy for the
// write lock before failing, so a contended write fails inside a test budget.
func openDBBusy(t *testing.T, path string, busy time.Duration) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Config{
		Path:        path,
		BusyTimeout: busy,
		Logger:      slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() }) // the handle is discarded here, so a close failure cannot fail the test
	return db
}

// holdWriteLock takes the database write lock through a second connection and
// holds it until the test ends, so a store write has to wait for it.
func holdWriteLock(t *testing.T, path string) {
	t.Helper()
	conn, err := openFresh(t, path).Conn(t.Context())
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) // the handle is discarded here, so a close failure cannot fail the test
	if _, err := conn.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("begin immediate: %v", err)
	}
	if _, err := conn.ExecContext(t.Context(),
		`UPDATE cost_budget SET spent_nd = spent_nd WHERE id = 1`); err != nil {
		t.Fatalf("take the write lock: %v", err)
	}
	t.Cleanup(func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") })
}

// seedCharges writes n charges through one fresh connection in one
// transaction, so a read has enough rows to take measurable time.
func seedCharges(t *testing.T, path string, n int) {
	t.Helper()
	tx, err := openFresh(t, path).Begin()
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := tx.Exec(
			`INSERT INTO cost_charge (kind, units, unit_price, ref, created_at) VALUES (?, ?, ?, ?, ?)`,
			"seed", 1, 1, "", 0); err != nil {
			t.Fatalf("seed charge: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
}

// seedReservations writes n live reservations through one fresh connection in
// one transaction, so a read has enough rows to take measurable time.
func seedReservations(t *testing.T, path string, n int) {
	t.Helper()
	tx, err := openFresh(t, path).Begin()
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	expires := time.Now().Add(time.Hour).UnixMilli()
	for i := 0; i < n; i++ {
		if _, err := tx.Exec(
			`INSERT INTO cost_reservation (id, amount_nd, expires_at, created_at) VALUES (?, ?, ?, ?)`,
			fmt.Sprintf("seed-%d", i), 1, expires, 0); err != nil {
			t.Fatalf("seed reservation: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
}

// seedExpiredReservations writes n reservations whose expiry has already
// passed through one fresh connection in one transaction, so an open has a
// backlog that its purge has to delete.
func seedExpiredReservations(t *testing.T, path string, n int) {
	t.Helper()
	tx, err := openFresh(t, path).Begin()
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	expires := time.Now().Add(-time.Hour).UnixMilli()
	for i := 0; i < n; i++ {
		if _, err := tx.Exec(
			`INSERT INTO cost_reservation (id, amount_nd, expires_at, created_at) VALUES (?, ?, ?, ?)`,
			fmt.Sprintf("expired-%d", i), 1, expires, 0); err != nil {
			t.Fatalf("seed expired reservation: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
}

// TestCancelledContextStopsEveryCall pins that a cancelled context stops every
// store call with an error wrapping context.Canceled, and that a cancelled
// write stores nothing. Without it the store could swallow cancellation,
// record a charge for a call that never happened, or report a zero as success.
func TestCancelledContextStopsEveryCall(t *testing.T) {
	store, db := openAt(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(1000))
	ctx := cancelledContext(t)
	calls := []struct {
		name string
		call func(context.Context) error
	}{
		{"Add", func(ctx context.Context) error {
			return store.Add(ctx, cost.Charge{Kind: "k", Units: 1, UnitPrice: 1})
		}},
		{"Charges", func(ctx context.Context) error { _, err := store.Charges(ctx); return err }},
		{"Total", func(ctx context.Context) error { _, err := store.Total(ctx); return err }},
		{"TotalByKind", func(ctx context.Context) error { _, err := store.TotalByKind(ctx, "k"); return err }},
		{"TotalForRef", func(ctx context.Context) error { _, err := store.TotalForRef(ctx, ""); return err }},
		{"Limit", func(ctx context.Context) error { _, err := store.Limit(ctx); return err }},
		{"Spent", func(ctx context.Context) error { _, err := store.Spent(ctx); return err }},
		{"Reserved", func(ctx context.Context) error { _, err := store.Reserved(ctx); return err }},
		{"Remaining", func(ctx context.Context) error { _, err := store.Remaining(ctx); return err }},
		{"Reserve", func(ctx context.Context) error { _, err := store.Reserve(ctx, 1); return err }},
		{"Settle", func(ctx context.Context) error { return store.Settle(ctx, sqlitestore.Reservation{}, 1) }},
		{"Release", func(ctx context.Context) error {
			return store.Release(ctx, sqlitestore.Reservation{ID: "hold"})
		}},
	}
	for _, c := range calls {
		if err := c.call(ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("%s with a cancelled context error = %v, want context.Canceled", c.name, err)
		}
	}
	if _, err := sqlitestore.Open(ctx, sqlitestore.Config{
		DB: db, Limit: cost.Dollar, Logger: slog.New(slog.DiscardHandler),
	}); !errors.Is(err, context.Canceled) {
		t.Errorf("open with a cancelled context error = %v, want context.Canceled", err)
	}
	if got, err := store.Charges(t.Context()); err != nil || len(got) != 0 {
		t.Fatalf("charges after cancelled writes = %v (err %v), want none", got, err)
	}
	if spent, err := store.Spent(t.Context()); err != nil || spent != 0 {
		t.Fatalf("spent after cancelled writes = %d (err %v), want 0", spent, err)
	}
	if reserved, err := store.Reserved(t.Context()); err != nil || reserved != 0 {
		t.Fatalf("reserved after cancelled writes = %d (err %v), want 0", reserved, err)
	}
}

// TestDeadlineInsideAWriteIsReported pins that a write whose context expires
// while the call runs reports the failure and stores nothing. The deadlines
// step across the whole call, so the cancellation lands between different
// statements from one call to the next. Without it a reserve or a settle that
// lost its context could commit a partial write the caller never sees, or
// report success for one that never landed.
func TestDeadlineInsideAWriteIsReported(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(1000000))
	ctx := t.Context()
	var (
		sawDeadline bool
		spent       cost.Price
	)
	for i := 0; i < 1200; i++ {
		deadline := time.Duration(i) * time.Microsecond

		reserveCtx, cancelReserve := context.WithTimeout(ctx, deadline)
		hold, err := store.Reserve(reserveCtx, 100)
		cancelReserve()
		switch {
		case err == nil:
			if err := store.Release(ctx, hold); err != nil {
				t.Fatalf("release the hold from the reserve with a %v deadline: %v", deadline, err)
			}
		default:
			sawDeadline = sawDeadline || errors.Is(err, context.DeadlineExceeded)
		}
		if reserved, err := store.Reserved(ctx); err != nil || reserved != 0 {
			t.Fatalf("reserve with a %v deadline reported %v and left %d reserved, want 0",
				deadline, err, reserved)
		}

		settleCtx, cancelSettle := context.WithTimeout(ctx, deadline)
		err = store.Settle(settleCtx, hold, 1)
		cancelSettle()
		if err == nil {
			spent++
		} else {
			sawDeadline = sawDeadline || errors.Is(err, context.DeadlineExceeded)
		}
		if booked, err := store.Spent(ctx); err != nil || booked != spent {
			t.Fatalf("settle with a %v deadline reported %v and booked %d, want %d",
				deadline, err, booked, spent)
		}
	}
	if !sawDeadline {
		t.Fatal("no write reported a context deadline, so the sweep never landed inside a call")
	}
}

// TestAContendedWriteIsReportedAndCommitsNothing pins that a store write which
// meets a held database write lock fails without storing anything. Without it
// a contended reserve or release could take a hold the caller never got or
// free one it still believes it holds.
func TestAContendedWriteIsReportedAndCommitsNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	const busy = 300 * time.Millisecond
	store, err := sqlitestore.Open(t.Context(), sqlitestore.Config{
		DB: openDBBusy(t, path, busy), Limit: 1000, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("open cost store: %v", err)
	}
	holdWriteLock(t, path)

	reserveCtx, cancelReserve := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancelReserve()
	if _, err := store.Reserve(reserveCtx, 100); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reserve behind a held lock error = %v, want context.DeadlineExceeded", err)
	}
	if reserved, err := store.Reserved(t.Context()); err != nil || reserved != 0 {
		t.Fatalf("reserved after the timed out reserve = %d (err %v), want 0", reserved, err)
	}

	releaseCtx, cancelRelease := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancelRelease()
	if err := store.Release(releaseCtx, sqlitestore.Reservation{ID: "held"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("release behind a held lock error = %v, want context.DeadlineExceeded", err)
	}

	openCtx, cancelOpen := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancelOpen()
	if _, err := sqlitestore.Open(openCtx, sqlitestore.Config{
		DB: openDBBusy(t, path, busy), Limit: 1000, Logger: slog.New(slog.DiscardHandler),
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("open behind a held lock error = %v, want context.DeadlineExceeded", err)
	}

	settleCtx, cancelSettle := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancelSettle()
	if err := store.Settle(settleCtx, sqlitestore.Reservation{ID: "held"}, 100); err == nil {
		t.Fatal("settle behind a held lock error = nil, want an error")
	}
	if spent, err := store.Spent(t.Context()); err != nil || spent != 0 {
		t.Fatalf("spent after the refused settle = %d (err %v), want 0", spent, err)
	}
}

// TestDeadlineInsideAReadIsReported pins that a read whose context expires
// while it walks stored rows reports the failure and returns no result at all.
// The deadlines step across a read of many rows, so the cancellation lands at
// different points of the walk. Without it a timed out read could return a
// partial ledger that a caller would take for the whole one.
func TestDeadlineInsideAReadIsReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	store := openStore(t, path, withLimit(1000000))
	const charges = 20000
	seedCharges(t, path, charges)
	const reservations = 40000
	seedReservations(t, path, reservations)

	var sawDeadline bool
	for _, deadline := range []time.Duration{
		200 * time.Microsecond, 500 * time.Microsecond, time.Millisecond,
		2 * time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond,
	} {
		ctx, cancel := context.WithTimeout(t.Context(), deadline)
		got, err := store.Charges(ctx)
		cancel()
		switch {
		case err != nil && len(got) != 0:
			t.Fatalf("charges with a %v deadline returned %d rows with %v, want none", deadline, len(got), err)
		case err != nil:
			sawDeadline = sawDeadline || errors.Is(err, context.DeadlineExceeded)
		case len(got) != charges:
			t.Fatalf("charges with a %v deadline returned %d rows with no error, want %d", deadline, len(got), charges)
		}

		ctx, cancel = context.WithTimeout(t.Context(), deadline)
		total, err := store.Total(ctx)
		cancel()
		if err != nil {
			sawDeadline = sawDeadline || errors.Is(err, context.DeadlineExceeded)
			if total != 0 {
				t.Fatalf("total with a %v deadline returned %d with %v, want 0", deadline, total, err)
			}
		} else if total != charges {
			t.Fatalf("total with a %v deadline returned %d with no error, want %d", deadline, total, charges)
		}

		ctx, cancel = context.WithTimeout(t.Context(), deadline)
		remaining, err := store.Remaining(ctx)
		cancel()
		if err != nil {
			sawDeadline = sawDeadline || errors.Is(err, context.DeadlineExceeded)
			if remaining != 0 {
				t.Fatalf("remaining with a %v deadline returned %d with %v, want 0", deadline, remaining, err)
			}
		} else if want := cost.Price(1000000 - reservations); remaining != want {
			t.Fatalf("remaining with a %v deadline returned %d with no error, want %d", deadline, remaining, want)
		}
	}
	if !sawDeadline {
		t.Fatal("no read reported a context deadline, so the sweep never landed inside a read")
	}
	if n := freshCount(t, path, "cost_charge"); n != charges {
		t.Fatalf("charges after the timed out reads = %d, want %d", n, charges)
	}
}

// TestOpenPurgeTimesOut pins that an open whose deadline lands while it purges
// an expired backlog fails and leaves the backlog in place. Without it a slow
// purge could report an open that never finished, or delete part of the
// backlog and still hand back a store.
func TestOpenPurgeTimesOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	openStore(t, path, withLimit(1000))
	const backlog = 25000
	seedExpiredReservations(t, path, backlog)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if _, err := sqlitestore.Open(ctx, sqlitestore.Config{
		DB: openDB(t, path), Limit: 1000, Logger: slog.New(slog.DiscardHandler),
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("open over an expired backlog error = %v, want context.DeadlineExceeded", err)
	}
	if n := freshCount(t, path, "cost_reservation"); n != backlog {
		t.Fatalf("reservations after the timed out open = %d, want %d", n, backlog)
	}
}

// TestNilLoggerUsesTheDefault pins that Open accepts a nil Logger and still
// works, which is the documented default. Without it a caller that omits the
// logger would panic while the store opened.
func TestNilLoggerUsesTheDefault(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), sqlitestore.Config{
		DB: openDB(t, filepath.Join(t.TempDir(), "cost.db")), Limit: 1000,
	})
	if err != nil {
		t.Fatalf("open with a nil logger: %v", err)
	}
	if remaining, err := store.Remaining(t.Context()); err != nil || remaining != 1000 {
		t.Fatalf("remaining with a nil logger = %d (err %v), want 1000", remaining, err)
	}
}

// TestDeepCreditOverflowsTheHeadroom pins that a credit deep enough to make the
// headroom unrepresentable is reported as cost.ErrOverflow, both when the
// headroom is read and when a hold is taken against it. Without it an
// overflowed headroom could be reported as a number, or let Reserve store a
// hold computed from it.
func TestDeepCreditOverflowsTheHeadroom(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(1000))
	ctx := t.Context()
	if err := store.Settle(ctx, sqlitestore.Reservation{}, math.MinInt64); err != nil {
		t.Fatalf("settle a deep credit: %v", err)
	}
	if spent, err := store.Spent(ctx); err != nil || spent != cost.Price(math.MinInt64) {
		t.Fatalf("spent after the deep credit = %d (err %v), want MinInt64", spent, err)
	}
	if _, err := store.Remaining(ctx); !errors.Is(err, cost.ErrOverflow) {
		t.Fatalf("remaining after the deep credit error = %v, want ErrOverflow", err)
	}
	if _, err := store.Reserve(ctx, 1); !errors.Is(err, cost.ErrOverflow) {
		t.Fatalf("reserve after the deep credit error = %v, want ErrOverflow", err)
	}
	if reserved, err := store.Reserved(ctx); err != nil || reserved != 0 {
		t.Fatalf("reserved after the refused reserve = %d (err %v), want 0", reserved, err)
	}
}

// costTablesOn lists the cost tables on disk at path over a connection the
// package never touches, then closes that connection before it returns.
func costTablesOn(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s for reading: %v", path, err)
	}
	defer func() { _ = db.Close() }() // the reader is discarded here, so a close failure cannot fail the test
	rows, err := db.Query(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'cost_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("list cost tables in %s: %v", path, err)
	}
	defer func() { _ = rows.Close() }() // the row cursor is drained before this returns
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan a cost table name in %s: %v", path, err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read cost tables in %s: %v", path, err)
	}
	return names
}

// assertWholeCostSchema checks that a cancelled open left a schema a retry can
// complete. The migration runner creates the ledger table first and applies
// each file in one transaction, so the cost tables on disk are either absent,
// the ledger alone, the first file's data tables alone, or every data table.
// A database that carries any table of the first file carries all of them,
// because one file commits as a unit, and it may or may not carry the owner
// table the second file adds.
func assertWholeCostSchema(t *testing.T, deadline int, tables []string) {
	t.Helper()
	if len(tables) == 0 {
		return
	}
	present := map[string]bool{}
	for _, name := range tables {
		present[name] = true
	}
	if !present["cost_schema_migrations"] {
		t.Fatalf("a %dus deadline left %v with no ledger table", deadline, tables)
	}
	firstFile := []string{"cost_charge", "cost_budget", "cost_reservation"}
	any := false
	for _, name := range firstFile {
		if present[name] {
			any = true
		}
	}
	if !any {
		return
	}
	for _, name := range firstFile {
		if !present[name] {
			t.Fatalf("a %dus deadline left a partial schema: %v", deadline, tables)
		}
	}
}

// TestCancelledOpenLeavesTheSchemaAndRetrySucceeds pins what a cancelled open
// leaves on disk. Open writes the schema and the ceiling before it returns, so
// a deadline that lands after the migration runner commits leaves the schema,
// its ledger row and the ceiling behind while Open reports no store. The test
// sweeps deadlines over fresh databases, lists the cost tables over a
// connection the package never touches, and retries each failed open with a
// live context. The leftover must be a whole schema state, the retry must
// succeed, and a second open on the same file must be unaffected.
func TestCancelledOpenLeavesTheSchemaAndRetrySucceeds(t *testing.T) {
	const sweep = 400
	dir := t.TempDir()
	var failed, deadlineFailed, leftLedger, leftFull int
	for i := 0; i < sweep; i++ {
		path := filepath.Join(dir, fmt.Sprintf("cancel%d.db", i))
		db, err := sqlite.Open(t.Context(), sqlite.Config{
			Path:   path,
			Logger: slog.New(slog.DiscardHandler),
		})
		if err != nil {
			t.Fatalf("open sqlite for sweep %d: %v", i, err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Duration(i)*time.Microsecond)
		store, err := sqlitestore.Open(ctx, sqlitestore.Config{
			DB: db, Limit: cost.Dollar, Logger: slog.New(slog.DiscardHandler),
		})
		cancel()
		if err == nil {
			if closeErr := db.Close(); closeErr != nil {
				t.Fatalf("close sqlite for sweep %d: %v", i, closeErr)
			}
			continue
		}
		failed++
		if store != nil {
			t.Fatalf("open with a %dus deadline failed and still returned a store", i)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			deadlineFailed++
		}
		tables := costTablesOn(t, path)
		assertWholeCostSchema(t, i, tables)
		switch len(tables) {
		case 0:
		case 1:
			leftLedger++
		default:
			leftFull++
		}
		// A second open on the same file, with a live context, must succeed and
		// report the ceiling, so the leftover schema is inert.
		retry, err := sqlitestore.Open(t.Context(), sqlitestore.Config{
			DB: db, Limit: cost.Dollar, Logger: slog.New(slog.DiscardHandler),
		})
		if err != nil {
			t.Fatalf("retry after a %dus failed open: %v", i, err)
		}
		if remaining, err := retry.Remaining(t.Context()); err != nil || remaining != cost.Dollar {
			t.Fatalf("retry after a %dus failed open remaining = %d (err %v), want %d",
				i, remaining, err, int64(cost.Dollar))
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close sqlite for sweep %d: %v", i, err)
		}
	}
	if failed == 0 {
		t.Fatal("no open failed with a deadline, so the sweep never cancelled an open")
	}
	if deadlineFailed == 0 {
		t.Fatal("no failed open reported the deadline, so the sweep never cancelled one mid statement")
	}
	t.Logf("failed opens=%d, reported the deadline=%d, left the ledger=%d, left the full schema=%d",
		failed, deadlineFailed, leftLedger, leftFull)
}
