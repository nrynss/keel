package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/sqlite"
)

// flightBase is the instant the flight tests stamp with. The clock never
// moves in these tests, because no expiry is under test here.
var flightBase = time.Date(2024, 3, 1, 12, 0, 0, 123456789, time.UTC)

// flightWork is a work body that measures its own price.
func flightWork(price cost.Price) cost.Work {
	return func(context.Context) (cost.Usage, error) {
		return cost.Usage{Price: price, Measured: true}, nil
	}
}

// openFlightStore opens a store for the flight tests and returns it with
// the channel the store sends a quote id on when a caller joins that
// quote's flight as a waiter. The join signal is what makes the wait under
// test deterministic, so no test has to guess when a waiter registered.
func openFlightStore(t *testing.T, path string) (*Store, <-chan string) {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Config{
		Path:   path,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() }) // the handle is discarded here, so a close failure cannot fail the test
	store, err := Open(t.Context(), Config{
		DB:     db,
		Limit:  100 * cost.Dollar,
		Now:    func() time.Time { return flightBase },
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("open cost store: %v", err)
	}
	joined := make(chan string, 1)
	store.onWaiterJoin = func(quoteID string) { joined <- quoteID }
	return store, joined
}

// flightOwnerLimit gives the flight tests' owner a ceiling to reserve
// under, ending the test if the ceiling cannot be written.
func flightOwnerLimit(t *testing.T, store *Store, owner string, limit cost.Price) {
	t.Helper()
	if err := NewKeyedBudget(store).SetLimit(t.Context(), owner, limit); err != nil {
		t.Fatalf("set owner limit %q: %v", owner, err)
	}
}

// flightOpen opens the file over a fresh connection, so an assertion reads
// what the store wrote and not what the store reports.
func flightOpen(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() }) // the handle is discarded here, so a close failure cannot fail the test
	return db
}

// flightRows counts every row in table over a fresh connection.
func flightRows(t *testing.T, path, table string) int {
	t.Helper()
	var n int
	if err := flightOpen(t, path).QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// flightState reads one quote's state over a fresh connection.
func flightState(t *testing.T, path, quoteID string) string {
	t.Helper()
	var state string
	if err := flightOpen(t, path).QueryRow(
		`SELECT state FROM cost_quote WHERE id = ?`, quoteID).Scan(&state); err != nil {
		t.Fatalf("read quote state %s: %v", quoteID, err)
	}
	return state
}

// TestRunConcurrentSharesOneOutcome pins that concurrent Runs of one quote
// produce one work execution, and the caller that waited receives the same
// outcome without a second charge. The join hook holds the leader back
// until the waiter is on the flight, so the wait itself is what carries
// the second outcome.
func TestRunConcurrentSharesOneOutcome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	store, joined := openFlightStore(t, path)
	ctx := t.Context()
	flightOwnerLimit(t, store, "alice", 10*cost.Dollar)
	quote, err := store.Quote(ctx, "alice", cost.Cent, time.Hour)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var runs atomic.Int32
	type result struct {
		usage cost.Usage
		err   error
	}
	results := make(chan result, 2)
	go func() {
		usage, err := store.Run(ctx, quote.ID, func(context.Context) (cost.Usage, error) {
			runs.Add(1)
			close(entered)
			<-release
			return cost.Usage{Price: 2 * cost.Cent, Measured: false}, nil
		})
		results <- result{usage, err}
	}()
	<-entered
	// The leader is parked in its work. Start the waiter and hold the
	// leader back until the hook reports the join, so the release can
	// never beat the registration.
	go func() {
		usage, err := store.Run(ctx, quote.ID, func(context.Context) (cost.Usage, error) {
			runs.Add(1)
			return cost.Usage{}, nil
		})
		results <- result{usage, err}
	}()
	if id := <-joined; id != quote.ID {
		t.Fatalf("a caller joined a flight for %q, want %q", id, quote.ID)
	}
	close(release)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent runs: %v, %v", first.err, second.err)
	}
	// The work reports no measurement, so the settle lands on the quoted
	// price, and both callers receive exactly that outcome.
	want := cost.Usage{Price: cost.Cent, Measured: false}
	if first.usage != want || second.usage != want {
		t.Fatalf("usages = %+v and %+v, want the shared outcome %+v", first.usage, second.usage, want)
	}
	if n := runs.Load(); n != 1 {
		t.Fatalf("work ran %d times, want once", n)
	}
	spent, err := store.Spent(ctx)
	if err != nil {
		t.Fatalf("read spent: %v", err)
	}
	if spent != cost.Cent {
		t.Fatalf("spent = %s, want one charge at the quoted price", spent)
	}
	if n := flightRows(t, path, "cost_charge"); n != 1 {
		t.Fatalf("charges = %d, want 1", n)
	}
}

// TestRunPanicFreesClaimAndAlertsWaiters pins the panic path. The claim is
// freed while the panic unwinds, the panic continues past Run, and a
// waiter receives its own error instead of a silent success or a
// forever-blocked call. The join hook holds the panic back until the
// waiter is on the flight, so the alert is deterministic.
func TestRunPanicFreesClaimAndAlertsWaiters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	store, joined := openFlightStore(t, path)
	ctx := t.Context()
	flightOwnerLimit(t, store, "alice", 10*cost.Dollar)
	quote, err := store.Quote(ctx, "alice", 5*cost.Cent, time.Hour)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		defer func() { _ = recover() }() // the panic belongs to this caller
		_, _ = store.Run(ctx, quote.ID, func(context.Context) (cost.Usage, error) {
			close(entered)
			<-release
			panic("boom")
		})
	}()
	<-entered
	waiterErr := make(chan error, 1)
	go func() {
		_, err := store.Run(ctx, quote.ID, flightWork(5*cost.Cent))
		waiterErr <- err
	}()
	if id := <-joined; id != quote.ID {
		t.Fatalf("a caller joined a flight for %q, want %q", id, quote.ID)
	}
	close(release)
	if err := <-waiterErr; !errors.Is(err, ErrRunAbandoned) {
		t.Fatalf("waiter error: %v, want ErrRunAbandoned", err)
	}
	<-leaderDone
	spent, err := store.Spent(ctx)
	if err != nil {
		t.Fatalf("read spent: %v", err)
	}
	if spent != 0 {
		t.Fatalf("spent = %s, want a panicked run to book nothing", spent)
	}
	if n := flightRows(t, path, "cost_reservation"); n != 0 {
		t.Fatalf("reservations = %d, want the panic to free the hold", n)
	}
	if state := flightState(t, path, quote.ID); state != "open" {
		t.Fatalf("panicked quote state = %q, want open", state)
	}
	usage, err := store.Run(ctx, quote.ID, flightWork(5*cost.Cent))
	if err != nil {
		t.Fatalf("run after the panic: %v", err)
	}
	if usage != (cost.Usage{Price: 5 * cost.Cent, Measured: true}) {
		t.Fatalf("usage = %+v", usage)
	}
}

// TestRunCancelledWaiterLeavesFlightAndLeaderBooks pins the waiter's own
// context. A cancelled waiter stops waiting and reports its own context
// error, while the leader completes, settles and records, so the waiter
// that departed misses the answer and never the booking.
func TestRunCancelledWaiterLeavesFlightAndLeaderBooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	store, joined := openFlightStore(t, path)
	ctx := t.Context()
	flightOwnerLimit(t, store, "alice", 10*cost.Dollar)
	quote, err := store.Quote(ctx, "alice", cost.Cent, time.Hour)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	leaderDone := make(chan error, 1)
	go func() {
		_, err := store.Run(ctx, quote.ID, func(context.Context) (cost.Usage, error) {
			close(entered)
			<-release
			return cost.Usage{Price: 2 * cost.Cent, Measured: false}, nil
		})
		leaderDone <- err
	}()
	<-entered
	waiterCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	waiterErr := make(chan error, 1)
	go func() {
		_, err := store.Run(waiterCtx, quote.ID, flightWork(cost.Cent))
		waiterErr <- err
	}()
	if id := <-joined; id != quote.ID {
		t.Fatalf("a caller joined a flight for %q, want %q", id, quote.ID)
	}
	cancel()
	if err := <-waiterErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter error: %v, want context.Canceled", err)
	}
	close(release)
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader run: %v", err)
	}
	// The outcome the departed waiter left behind is in the ledger, and
	// a later Run of the id answers with it instead of running again.
	spent, err := store.Spent(ctx)
	if err != nil {
		t.Fatalf("read spent: %v", err)
	}
	if spent != cost.Cent {
		t.Fatalf("spent = %s, want the booking the waiter left behind", spent)
	}
	usage, err := store.Run(ctx, quote.ID, func(context.Context) (cost.Usage, error) {
		t.Fatal("the run after the departure ran the work")
		return cost.Usage{}, nil
	})
	if err != nil {
		t.Fatalf("run after the departure: %v", err)
	}
	if usage != (cost.Usage{Price: cost.Cent, Measured: false}) {
		t.Fatalf("usage = %+v, want the recorded outcome", usage)
	}
}
