package sqlitestore_test

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/cost/sqlitestore"
)

// testWork is a work body that measures its own price.
func testWork(price cost.Price) cost.Work {
	return func(context.Context) (cost.Usage, error) {
		return cost.Usage{Price: price, Measured: true}, nil
	}
}

// failedWork is a work body that fails with its own error.
func failedWork(err error) cost.Work {
	return func(context.Context) (cost.Usage, error) {
		return cost.Usage{}, err
	}
}

// setOwnerLimit gives owner a ceiling to reserve under, ending the test if
// the ceiling cannot be written.
func setOwnerLimit(t *testing.T, keyed *sqlitestore.KeyedBudget, owner string, limit cost.Price) {
	t.Helper()
	if err := keyed.SetLimit(t.Context(), owner, limit); err != nil {
		t.Fatalf("set owner limit %q: %v", owner, err)
	}
}

// freshQuoteState reads one quote's state over a fresh connection.
func freshQuoteState(t *testing.T, path, quoteID string) string {
	t.Helper()
	var state string
	if err := openFresh(t, path).QueryRow(
		`SELECT state FROM cost_quote WHERE id = ?`, quoteID).Scan(&state); err != nil {
		t.Fatalf("read quote state %s: %v", quoteID, err)
	}
	return state
}

// freshQuoteCount counts the rows one quote id has over a fresh connection.
func freshQuoteCount(t *testing.T, path, quoteID string) int {
	t.Helper()
	var n int
	if err := openFresh(t, path).QueryRow(
		`SELECT COUNT(*) FROM cost_quote WHERE id = ?`, quoteID).Scan(&n); err != nil {
		t.Fatalf("count quote %s: %v", quoteID, err)
	}
	return n
}

// freshCharge pins the one charge a quote booked, over a fresh connection.
func freshCharge(t *testing.T, path, ref string) (kind string, unitPrice int64, denom string) {
	t.Helper()
	var n int
	if err := openFresh(t, path).QueryRow(
		`SELECT COUNT(*) FROM cost_charge WHERE ref = ?`, ref).Scan(&n); err != nil {
		t.Fatalf("count charges for %s: %v", ref, err)
	}
	if n != 1 {
		t.Fatalf("charges for %s = %d, want 1", ref, n)
	}
	if err := openFresh(t, path).QueryRow(
		`SELECT kind, unit_price, denomination FROM cost_charge WHERE ref = ?`, ref).Scan(&kind, &unitPrice, &denom); err != nil {
		t.Fatalf("read charge for %s: %v", ref, err)
	}
	return kind, unitPrice, denom
}

// refusalOf fails the test unless err is a quote refusal, and returns it.
func refusalOf(t *testing.T, err error) *cost.QuoteRefusal {
	t.Helper()
	var refusal *cost.QuoteRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("run quote: got %v, want a QuoteRefusal", err)
	}
	return refusal
}

// spentOf reads the store's booked spend.
func spentOf(t *testing.T, store *sqlitestore.Store) cost.Price {
	t.Helper()
	spent, err := store.Spent(t.Context())
	if err != nil {
		t.Fatalf("read spent: %v", err)
	}
	return spent
}

// TestQuoteRunsOnceAndReplaysOutcome pins the whole happy path. A quote
// holds no budget, one Run charges once through the meter's shape, a
// second Run returns the first outcome and charges nothing, and the
// outcome survives a reopen of the same file.
func TestQuoteRunsOnceAndReplaysOutcome(t *testing.T) {
	clock := &testClock{at: base}
	path := filepath.Join(t.TempDir(), "cost.db")
	store, db := openAt(t, path, withClock(clock.now), withLimit(100*cost.Dollar))
	ctx := t.Context()
	setOwnerLimit(t, sqlitestore.NewKeyedBudget(store), "alice", 10*cost.Dollar)

	quote, err := store.Quote(ctx, "alice", 5*cost.Cent, time.Hour)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if len(quote.ID) != 32 {
		t.Fatalf("quote id %q is not 32 characters", quote.ID)
	}
	if quote.Owner != "alice" || quote.Price != 5*cost.Cent {
		t.Fatalf("quote = %+v", quote)
	}
	if quote.Denomination != (cost.Denomination{}) {
		t.Fatalf("quote denomination = %+v, want the zero value", quote.Denomination)
	}
	if !quote.ExpiresAt.Equal(base.Add(time.Hour)) {
		t.Fatalf("quote expires at %v, want %v", quote.ExpiresAt, base.Add(time.Hour))
	}
	reserved, err := store.Reserved(ctx)
	if err != nil {
		t.Fatalf("read reserved: %v", err)
	}
	if reserved != 0 {
		t.Fatalf("quote holds %s of budget, want none", reserved)
	}
	if n := freshCount(t, path, "cost_reservation"); n != 0 {
		t.Fatalf("quote left %d reservation rows", n)
	}

	const charged = 4 * cost.Cent
	usage, err := store.Run(ctx, quote.ID, testWork(charged))
	if err != nil {
		t.Fatalf("run quote: %v", err)
	}
	if usage != (cost.Usage{Price: charged, Measured: true}) {
		t.Fatalf("usage = %+v, want the measured price", usage)
	}
	if spent := spentOf(t, store); spent != charged {
		t.Fatalf("spent = %s, want %s", spent, charged)
	}
	kind, unitPrice, denom := freshCharge(t, path, quote.ID)
	if kind != "quoted" || unitPrice != int64(charged) || denom != "" {
		t.Fatalf("charge = (%q, %d, %q)", kind, unitPrice, denom)
	}

	usage, err = store.Run(ctx, quote.ID, func(context.Context) (cost.Usage, error) {
		t.Fatal("the second run ran the work")
		return cost.Usage{}, nil
	})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if usage != (cost.Usage{Price: charged, Measured: true}) {
		t.Fatalf("second usage = %+v, want the first outcome", usage)
	}
	if spent := spentOf(t, store); spent != charged {
		t.Fatalf("spent after the replay = %s, want %s", spent, charged)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened := openStore(t, path, withClock(clock.now), withLimit(100*cost.Dollar))
	usage, err = reopened.Run(ctx, quote.ID, func(context.Context) (cost.Usage, error) {
		t.Fatal("the run after reopen ran the work")
		return cost.Usage{}, nil
	})
	if err != nil {
		t.Fatalf("run after reopen: %v", err)
	}
	if usage != (cost.Usage{Price: charged, Measured: true}) {
		t.Fatalf("usage after reopen = %+v, want the recorded outcome", usage)
	}
	if spent := spentOf(t, reopened); spent != charged {
		t.Fatalf("spent after reopen = %s, want %s", spent, charged)
	}
}

// TestRunAfterClaimFromFreshHandleChargesOnce simulates a crash between
// claim and work. A fresh handle finds the live claim and refuses without
// running the work, then finds the lapsed claim gone, and the whole retry
// sequence never books a second charge.
func TestRunAfterClaimFromFreshHandleChargesOnce(t *testing.T) {
	clock := &testClock{at: base}
	path := filepath.Join(t.TempDir(), "cost.db")
	storeA, _ := openAt(t, path, withClock(clock.now), withTTL(time.Minute), withLimit(100*cost.Dollar))
	ctx := t.Context()
	setOwnerLimit(t, sqlitestore.NewKeyedBudget(storeA), "alice", 10*cost.Dollar)
	quote, err := storeA.Quote(ctx, "alice", 5*cost.Cent, time.Hour)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	doneA := make(chan error, 1)
	go func() {
		_, err := storeA.Run(ctx, quote.ID, func(context.Context) (cost.Usage, error) {
			close(entered)
			<-release
			return cost.Usage{Price: 5 * cost.Cent, Measured: true}, nil
		})
		doneA <- err
	}()
	<-entered

	storeB := openStore(t, path, withClock(clock.now), withTTL(time.Minute), withLimit(100*cost.Dollar))
	var freshRan atomic.Int32
	freshWork := func(context.Context) (cost.Usage, error) {
		freshRan.Add(1)
		return cost.Usage{}, nil
	}
	_, err = storeB.Run(ctx, quote.ID, freshWork)
	if refusal := refusalOf(t, err); refusal.Code != cost.CodeQuotePending {
		t.Fatalf("live claim refused with %q, want quote_pending", refusal.Code)
	}
	if n := freshRan.Load(); n != 0 {
		t.Fatalf("the fresh handle ran the work %d times", n)
	}
	if spent := spentOf(t, storeB); spent != 0 {
		t.Fatalf("spent = %s, want none", spent)
	}

	// The claim lapses, so the runner that held it is gone and the id
	// reads as unknown. Still nothing ran and nothing booked.
	clock.advance(2 * time.Minute)
	_, err = storeB.Run(ctx, quote.ID, freshWork)
	if refusal := refusalOf(t, err); refusal.Code != cost.CodeQuoteUnknown {
		t.Fatalf("lapsed claim refused with %q, want quote_unknown", refusal.Code)
	}
	if n := freshRan.Load(); n != 0 {
		t.Fatalf("the lapsed retry ran the work %d times", n)
	}
	if n := freshCount(t, path, "cost_charge"); n != 0 {
		t.Fatalf("charges = %d, want none", n)
	}

	// The original runner finishes and settles exactly once, and the
	// recorded outcome answers every later Run of the same id.
	close(release)
	if err := <-doneA; err != nil {
		t.Fatalf("original run: %v", err)
	}
	usage, err := storeB.Run(ctx, quote.ID, freshWork)
	if err != nil {
		t.Fatalf("run after the settle: %v", err)
	}
	if usage != (cost.Usage{Price: 5 * cost.Cent, Measured: true}) {
		t.Fatalf("usage = %+v, want the settled outcome", usage)
	}
	if n := freshRan.Load(); n != 0 {
		t.Fatalf("the finished quote ran the work %d times", n)
	}
	if spent := spentOf(t, storeB); spent != 5*cost.Cent {
		t.Fatalf("spent = %s, want exactly one charge", spent)
	}
	if n := freshCount(t, path, "cost_charge"); n != 1 {
		t.Fatalf("charges = %d, want 1", n)
	}
	if n := freshCount(t, path, "cost_reservation"); n != 0 {
		t.Fatalf("reservations = %d, want the settle to free the hold", n)
	}
}

// TestRunExpiredQuoteIssuesFreshQuote pins the expiry refusal and that the
// quote it carries is a real, runnable quote at the same price.
func TestRunExpiredQuoteIssuesFreshQuote(t *testing.T) {
	clock := &testClock{at: base}
	path := filepath.Join(t.TempDir(), "cost.db")
	store := openStore(t, path, withClock(clock.now), withLimit(100*cost.Dollar))
	ctx := t.Context()
	setOwnerLimit(t, sqlitestore.NewKeyedBudget(store), "alice", 10*cost.Dollar)
	quote, err := store.Quote(ctx, "alice", 5*cost.Cent, time.Hour)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	clock.advance(2 * time.Hour)

	_, err = store.Run(ctx, quote.ID, testWork(5*cost.Cent))
	refusal := refusalOf(t, err)
	if refusal.Code != cost.CodeQuoteExpired {
		t.Fatalf("code = %q, want quote_expired", refusal.Code)
	}
	if !errors.Is(err, cost.ErrQuoteExpired) {
		t.Fatalf("run quote: %v, want ErrQuoteExpired", err)
	}
	if refusal.Quote.ID == "" || refusal.Quote.ID == quote.ID {
		t.Fatalf("fresh quote id = %q, want a new id", refusal.Quote.ID)
	}
	if refusal.Quote.Price != 5*cost.Cent {
		t.Fatalf("fresh price = %s, want the quoted price", refusal.Quote.Price)
	}
	if !refusal.Quote.ExpiresAt.Equal(base.Add(3 * time.Hour)) {
		t.Fatalf("fresh quote expires at %v, want one full window", refusal.Quote.ExpiresAt)
	}
	if n := freshQuoteCount(t, path, quote.ID); n != 0 {
		t.Fatalf("the expired quote left %d rows, want the sweep to take it", n)
	}
	if state := freshQuoteState(t, path, refusal.Quote.ID); state != "open" {
		t.Fatalf("fresh quote state = %q, want open", state)
	}

	usage, err := store.Run(ctx, refusal.Quote.ID, testWork(5*cost.Cent))
	if err != nil {
		t.Fatalf("run the fresh quote: %v", err)
	}
	if usage != (cost.Usage{Price: 5 * cost.Cent, Measured: true}) {
		t.Fatalf("usage = %+v", usage)
	}
	if spent := spentOf(t, store); spent != 5*cost.Cent {
		t.Fatalf("spent = %s, want the fresh quote's charge", spent)
	}
}

// TestRunExpiredQuoteRepricesBeforeRefusing pins that the price check
// covers an open row past its confirm window, where its price is the
// oldest. An unmoved price refuses with quote_expired and a fresh quote at
// the price Reprice reported, not the recorded one. A moved price refuses
// with quote_price_moved and a fresh quote at the current price, exactly
// like a live quote. The cases run on separate files, because every write
// a quote drives sweeps the expired rows it finds on the way.
func TestRunExpiredQuoteRepricesBeforeRefusing(t *testing.T) {
	clock := &testClock{at: base}
	price := 10 * cost.Cent
	newStore := func(t *testing.T, path string) *sqlitestore.Store {
		store := openStore(t, path, withClock(clock.now), withLimit(100*cost.Dollar),
			func(c *sqlitestore.Config) {
				c.Reprice = func(context.Context, string) (cost.Price, error) { return price, nil }
				c.QuoteTolerance = cost.Cent
			})
		setOwnerLimit(t, sqlitestore.NewKeyedBudget(store), "alice", 10*cost.Dollar)
		return store
	}
	ctx := t.Context()

	// Past its window with the price inside the tolerance, the refusal
	// is the expiry one, and the fresh quote carries the repriced
	// price rather than the one the row recorded.
	path := filepath.Join(t.TempDir(), "cost.db")
	store := newStore(t, path)
	unmoved, err := store.Quote(ctx, "alice", 10*cost.Cent, time.Hour)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	clock.advance(2 * time.Hour)
	price = 10*cost.Cent + cost.Cent/2
	_, err = store.Run(ctx, unmoved.ID, testWork(price))
	refusal := refusalOf(t, err)
	if refusal.Code != cost.CodeQuoteExpired {
		t.Fatalf("code = %q, want quote_expired", refusal.Code)
	}
	if !errors.Is(err, cost.ErrQuoteExpired) {
		t.Fatalf("run quote: %v, want ErrQuoteExpired", err)
	}
	if refusal.Quote.Price != price {
		t.Fatalf("fresh price = %s, want the repriced price %s", refusal.Quote.Price, price)
	}
	if n := freshQuoteCount(t, path, unmoved.ID); n != 0 {
		t.Fatalf("the expired quote left %d rows, want the sweep to take it", n)
	}
	usage, err := store.Run(ctx, refusal.Quote.ID, testWork(price))
	if err != nil {
		t.Fatalf("run the fresh quote: %v", err)
	}
	if usage != (cost.Usage{Price: price, Measured: true}) {
		t.Fatalf("usage = %+v", usage)
	}

	// A price that moved past the window refuses as moved, with a
	// fresh quote at the moved price, and books nothing on the refusal.
	path = filepath.Join(t.TempDir(), "cost.db")
	store = newStore(t, path)
	moved, err := store.Quote(ctx, "alice", 10*cost.Cent, time.Hour)
	if err != nil {
		t.Fatalf("second quote: %v", err)
	}
	clock.advance(2 * time.Hour)
	price = 12 * cost.Cent
	_, err = store.Run(ctx, moved.ID, testWork(price))
	refusal = refusalOf(t, err)
	if refusal.Code != cost.CodeQuotePriceMoved {
		t.Fatalf("code = %q, want quote_price_moved", refusal.Code)
	}
	if !errors.Is(err, cost.ErrQuotePriceMoved) {
		t.Fatalf("run quote: %v, want ErrQuotePriceMoved", err)
	}
	if refusal.Quote.Price != 12*cost.Cent {
		t.Fatalf("fresh price = %s, want the moved price", refusal.Quote.Price)
	}
	if n := freshQuoteCount(t, path, moved.ID); n != 0 {
		t.Fatalf("the refused quote left %d rows, want the sweep to take it", n)
	}
	if state := freshQuoteState(t, path, refusal.Quote.ID); state != "open" {
		t.Fatalf("fresh quote state = %q, want open", state)
	}
	if spent := spentOf(t, store); spent != 0 {
		t.Fatalf("spent = %s, want the refusal to book nothing", spent)
	}
	usage, err = store.Run(ctx, refusal.Quote.ID, testWork(12*cost.Cent))
	if err != nil {
		t.Fatalf("run the fresh quote: %v", err)
	}
	if usage != (cost.Usage{Price: 12 * cost.Cent, Measured: true}) {
		t.Fatalf("usage = %+v", usage)
	}
	if spent := spentOf(t, store); spent != 12*cost.Cent {
		t.Fatalf("spent = %s, want one charge at the moved price", spent)
	}
}

// TestRunPriceMovedPastTolerance pins the price movement refusal. A move
// within the tolerance, including exactly to it, proceeds. A move past it
// refuses before any spend and carries a fresh quote at the current price.
func TestRunPriceMovedPastTolerance(t *testing.T) {
	clock := &testClock{at: base}
	path := filepath.Join(t.TempDir(), "cost.db")
	price := 10 * cost.Cent
	store := openStore(t, path, withClock(clock.now), withLimit(100*cost.Dollar),
		func(c *sqlitestore.Config) {
			c.Reprice = func(context.Context, string) (cost.Price, error) { return price, nil }
			c.QuoteTolerance = cost.Cent
		})
	ctx := t.Context()
	setOwnerLimit(t, sqlitestore.NewKeyedBudget(store), "alice", 10*cost.Dollar)

	quote, err := store.Quote(ctx, "alice", 10*cost.Cent, time.Hour)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	price = 11 * cost.Cent
	usage, err := store.Run(ctx, quote.ID, testWork(11*cost.Cent))
	if err != nil {
		t.Fatalf("run at the tolerance boundary: %v", err)
	}
	if usage != (cost.Usage{Price: 11 * cost.Cent, Measured: true}) {
		t.Fatalf("usage = %+v", usage)
	}

	moved, err := store.Quote(ctx, "alice", 10*cost.Cent, time.Hour)
	if err != nil {
		t.Fatalf("second quote: %v", err)
	}
	price = 12 * cost.Cent
	_, err = store.Run(ctx, moved.ID, testWork(12*cost.Cent))
	refusal := refusalOf(t, err)
	if refusal.Code != cost.CodeQuotePriceMoved {
		t.Fatalf("code = %q, want quote_price_moved", refusal.Code)
	}
	if !errors.Is(err, cost.ErrQuotePriceMoved) {
		t.Fatalf("run quote: %v, want ErrQuotePriceMoved", err)
	}
	if refusal.Quote.Price != 12*cost.Cent {
		t.Fatalf("fresh price = %s, want the moved price", refusal.Quote.Price)
	}
	if state := freshQuoteState(t, path, moved.ID); state != "open" {
		t.Fatalf("refused quote state = %q, want open", state)
	}
	if spent := spentOf(t, store); spent != 11*cost.Cent {
		t.Fatalf("spent = %s, want the refusal to book nothing", spent)
	}

	usage, err = store.Run(ctx, refusal.Quote.ID, testWork(12*cost.Cent))
	if err != nil {
		t.Fatalf("run the fresh quote: %v", err)
	}
	if usage != (cost.Usage{Price: 12 * cost.Cent, Measured: true}) {
		t.Fatalf("usage = %+v", usage)
	}
	if spent := spentOf(t, store); spent != 23*cost.Cent {
		t.Fatalf("spent = %s, want both confirmed runs", spent)
	}
	if n := freshCount(t, path, "cost_charge"); n != 2 {
		t.Fatalf("charges = %d, want 2", n)
	}
}

// TestQuoteCarriesDenominationAndRefusesMismatch pins that a credit store
// quotes in its own denomination, and a row stored under another unit
// refuses before anything books.
func TestQuoteCarriesDenominationAndRefusesMismatch(t *testing.T) {
	clock := &testClock{at: base}
	path := filepath.Join(t.TempDir(), "cost.db")
	store := openStore(t, path, withClock(clock.now), withLimit(100*cost.Dollar), withDenomination(credit))
	ctx := t.Context()
	setOwnerLimit(t, sqlitestore.NewKeyedBudget(store), "alice", 10*cost.Dollar)
	quote, err := store.Quote(ctx, "alice", 5, time.Hour)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if quote.Denomination != credit {
		t.Fatalf("quote denomination = %+v, want %+v", quote.Denomination, credit)
	}
	if _, err := openFresh(t, path).Exec(
		`UPDATE cost_quote SET denomination = 'other' WHERE id = ?`, quote.ID); err != nil {
		t.Fatalf("tamper the row: %v", err)
	}
	_, err = store.Run(ctx, quote.ID, testWork(4))
	if !errors.Is(err, cost.ErrDenominationMismatch) {
		t.Fatalf("run quote: %v, want ErrDenominationMismatch", err)
	}
	if spent := spentOf(t, store); spent != 0 {
		t.Fatalf("spent = %s, want none", spent)
	}
	if n := freshCount(t, path, "cost_charge"); n != 0 {
		t.Fatalf("charges = %d, want none", n)
	}
}

// TestSweepRemovesExpiredQuotes pins the sweep. Open and done quotes die
// at their confirm deadline, live quotes survive, and a reopen sweeps what
// expired while the store was closed.
func TestSweepRemovesExpiredQuotes(t *testing.T) {
	clock := &testClock{at: base}
	path := filepath.Join(t.TempDir(), "cost.db")
	store, db := openAt(t, path, withClock(clock.now), withLimit(100*cost.Dollar))
	ctx := t.Context()
	setOwnerLimit(t, sqlitestore.NewKeyedBudget(store), "alice", 10*cost.Dollar)
	expired, err := store.Quote(ctx, "alice", cost.Cent, time.Hour)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	live, err := store.Quote(ctx, "alice", cost.Cent, 10*time.Hour)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	done, err := store.Quote(ctx, "alice", cost.Cent, 2*time.Hour)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if _, err := store.Run(ctx, done.ID, testWork(cost.Cent)); err != nil {
		t.Fatalf("run: %v", err)
	}
	clock.advance(3 * time.Hour)
	if _, err := store.Quote(ctx, "alice", cost.Cent, time.Hour); err != nil {
		t.Fatalf("quote: %v", err)
	}
	if n := freshQuoteCount(t, path, expired.ID); n != 0 {
		t.Fatalf("the expired quote left %d rows", n)
	}
	if n := freshQuoteCount(t, path, done.ID); n != 0 {
		t.Fatalf("the finished quote left %d rows past its deadline", n)
	}
	if n := freshQuoteCount(t, path, live.ID); n != 1 {
		t.Fatalf("the live quote has %d rows", n)
	}
	clock.advance(20 * time.Hour)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened := openStore(t, path, withClock(clock.now), withLimit(100*cost.Dollar))
	if n := freshQuoteCount(t, path, live.ID); n != 0 {
		t.Fatalf("the reopen left %d rows of a quote that expired", n)
	}
	if spent := spentOf(t, reopened); spent != cost.Cent {
		t.Fatalf("spent = %s, want the sweep to keep the ledger", spent)
	}
}

// TestRunAndQuoteRefusals pins the argument checks and the two budget
// refusals the claim enforces. A refused claim leaves the quote open, so a
// corrected ceiling lets the same id through.
func TestRunAndQuoteRefusals(t *testing.T) {
	clock := &testClock{at: base}
	path := filepath.Join(t.TempDir(), "cost.db")
	store := openStore(t, path, withClock(clock.now), withLimit(100*cost.Dollar))
	ctx := t.Context()
	keyed := sqlitestore.NewKeyedBudget(store)

	if _, err := store.Run(ctx, "", testWork(cost.Cent)); !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Fatalf("empty id: %v, want ErrInvalid", err)
	}
	if _, err := store.Run(ctx, "missing", testWork(cost.Cent)); refusalOf(t, err).Code != cost.CodeQuoteUnknown {
		t.Fatalf("unknown id did not refuse with quote_unknown: %v", err)
	}
	if _, err := store.Quote(ctx, "", cost.Cent, time.Hour); !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Fatalf("empty owner: %v, want ErrInvalid", err)
	}
	if _, err := store.Quote(ctx, "alice", -cost.Cent, time.Hour); !errors.Is(err, cost.ErrNegativeEstimate) {
		t.Fatalf("negative estimate: %v, want ErrNegativeEstimate", err)
	}
	if _, err := store.Quote(ctx, "alice", cost.Cent, 0); !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Fatalf("zero ttl: %v, want ErrInvalid", err)
	}
	db := openDB(t, path)
	_, err := sqlitestore.Open(ctx, sqlitestore.Config{
		DB:             db,
		Limit:          cost.Dollar,
		QuoteTolerance: -1,
		Logger:         slog.New(slog.DiscardHandler),
	})
	if !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Fatalf("negative tolerance: %v, want ErrInvalid", err)
	}

	quote, err := store.Quote(ctx, "nobody", cost.Cent, time.Hour)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	_, err = store.Run(ctx, quote.ID, testWork(cost.Cent))
	if !errors.Is(err, cost.ErrUnknownOwner) {
		t.Fatalf("unknown owner: %v, want ErrUnknownOwner", err)
	}
	if state := freshQuoteState(t, path, quote.ID); state != "open" {
		t.Fatalf("refused quote state = %q, want open", state)
	}

	setOwnerLimit(t, keyed, "alice", cost.Cent)
	small, err := store.Quote(ctx, "alice", 5*cost.Cent, time.Hour)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	_, err = store.Run(ctx, small.ID, testWork(5*cost.Cent))
	if !errors.Is(err, cost.ErrOverBudget) {
		t.Fatalf("over budget: %v, want ErrOverBudget", err)
	}
	setOwnerLimit(t, keyed, "alice", 10*cost.Dollar)
	usage, err := store.Run(ctx, small.ID, testWork(5*cost.Cent))
	if err != nil {
		t.Fatalf("run after the raise: %v", err)
	}
	if usage != (cost.Usage{Price: 5 * cost.Cent, Measured: true}) {
		t.Fatalf("usage = %+v", usage)
	}
}

// TestWorkFailureReopensQuote pins that an observed failure frees the hold
// and returns the quote to open, so the same id retries and charges only
// for the run that succeeded.
func TestWorkFailureReopensQuote(t *testing.T) {
	clock := &testClock{at: base}
	path := filepath.Join(t.TempDir(), "cost.db")
	store := openStore(t, path, withClock(clock.now), withLimit(100*cost.Dollar))
	ctx := t.Context()
	setOwnerLimit(t, sqlitestore.NewKeyedBudget(store), "alice", 10*cost.Dollar)
	quote, err := store.Quote(ctx, "alice", 5*cost.Cent, time.Hour)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	sentinel := errors.New("render failed")
	_, err = store.Run(ctx, quote.ID, failedWork(sentinel))
	if !errors.Is(err, sentinel) {
		t.Fatalf("run quote: %v, want the work's error", err)
	}
	if spent := spentOf(t, store); spent != 0 {
		t.Fatalf("spent = %s, want a failed run to book nothing", spent)
	}
	if n := freshCount(t, path, "cost_reservation"); n != 0 {
		t.Fatalf("reservations = %d, want the failure to free the hold", n)
	}
	if state := freshQuoteState(t, path, quote.ID); state != "open" {
		t.Fatalf("failed quote state = %q, want open", state)
	}
	usage, err := store.Run(ctx, quote.ID, testWork(4*cost.Cent))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if usage != (cost.Usage{Price: 4 * cost.Cent, Measured: true}) {
		t.Fatalf("usage = %+v", usage)
	}
	if spent := spentOf(t, store); spent != 4*cost.Cent {
		t.Fatalf("spent = %s, want one charge for the successful run", spent)
	}
}

// TestRunSettlesAfterCallerCancel pins both sides of the cancellation
// contract. A context cancelled before the claim stops the run and books
// nothing. A context that dies once the work has run still leaves the
// outcome recorded, because the spend the work caused is real.
func TestRunSettlesAfterCallerCancel(t *testing.T) {
	clock := &testClock{at: base}
	path := filepath.Join(t.TempDir(), "cost.db")
	store := openStore(t, path, withClock(clock.now), withLimit(100*cost.Dollar))
	ctx := t.Context()
	setOwnerLimit(t, sqlitestore.NewKeyedBudget(store), "alice", 10*cost.Dollar)

	dead, err := store.Quote(ctx, "alice", cost.Cent, time.Hour)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	deadCtx, cancelDead := context.WithCancel(ctx)
	cancelDead()
	if _, err := store.Run(deadCtx, dead.ID, func(context.Context) (cost.Usage, error) {
		t.Fatal("the run of a cancelled context ran the work")
		return cost.Usage{}, nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("run on a cancelled context: %v, want context.Canceled", err)
	}
	if spent := spentOf(t, store); spent != 0 {
		t.Fatalf("spent = %s, want a run that never claimed to book nothing", spent)
	}
	if state := freshQuoteState(t, path, dead.ID); state != "open" {
		t.Fatalf("refused quote state = %q, want open", state)
	}

	quote, err := store.Quote(ctx, "alice", 5*cost.Cent, time.Hour)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	usage, err := store.Run(runCtx, quote.ID, func(context.Context) (cost.Usage, error) {
		cancel()
		return cost.Usage{Price: 4 * cost.Cent, Measured: true}, nil
	})
	if err != nil {
		t.Fatalf("run past a caller that left: %v", err)
	}
	if usage != (cost.Usage{Price: 4 * cost.Cent, Measured: true}) {
		t.Fatalf("usage = %+v, want the settled outcome", usage)
	}
	if spent := spentOf(t, store); spent != 4*cost.Cent {
		t.Fatalf("spent = %s, want the spend the work caused to book", spent)
	}
	usage, err = store.Run(ctx, quote.ID, func(context.Context) (cost.Usage, error) {
		t.Fatal("the run after the settle ran the work")
		return cost.Usage{}, nil
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if usage != (cost.Usage{Price: 4 * cost.Cent, Measured: true}) {
		t.Fatalf("replayed usage = %+v, want the recorded outcome", usage)
	}
}
