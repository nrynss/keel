package sqlitestore_test

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/cost/sqlitestore"
)

// credit is the denomination the grant tests speak. The name is opaque and
// the minor unit is one, because the credit only exists whole.
var credit = cost.Denomination{Name: "provider.unit", Minor: 1}

// withDenomination sets the store's denomination.
func withDenomination(d cost.Denomination) func(*sqlitestore.Config) {
	return func(c *sqlitestore.Config) { c.Denomination = d }
}

// mustCreditStore opens a store that speaks the credit denomination, with a
// ceiling high enough that the grants alone bind every refusal below.
func mustCreditStore(t *testing.T, path string, clock *testClock, opts ...func(*sqlitestore.Config)) *sqlitestore.Store {
	t.Helper()
	base := []func(*sqlitestore.Config){
		withLimit(1 << 40), withClock(clock.now), withDenomination(credit),
	}
	return openStore(t, path, append(base, opts...)...)
}

// mustGrant posts a grant or ends the test.
func mustGrant(t *testing.T, store *sqlitestore.Store, amount cost.Price, expiresAt time.Time, key string) {
	t.Helper()
	if err := store.Grant(t.Context(), amount, expiresAt, key); err != nil {
		t.Fatalf("grant %d key %q: %v", amount, key, err)
	}
}

// mustBalance reads the live grant balance or ends the test.
func mustBalance(t *testing.T, store *sqlitestore.Store) cost.Price {
	t.Helper()
	balance, err := store.Balance(t.Context())
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	return balance
}

// grantRow is the raw state of one grant, read over a connection the package
// never touches.
type grantRow struct {
	amount    int64
	drawn     int64
	key       string
	expiresAt int64
}

// readGrants lists the grant rows at path over a fresh connection, ordered by
// expiry, so the pins below stand on the stored bytes and not on the store's
// own reports.
func readGrants(t *testing.T, path string) []grantRow {
	t.Helper()
	rows, err := openFresh(t, path).Query(
		`SELECT amount_nd, drawn_nd, grant_key, expires_at FROM cost_grant ORDER BY expires_at, id`)
	if err != nil {
		t.Fatalf("read grants: %v", err)
	}
	defer func() { _ = rows.Close() }() // the cursor is drained before this returns
	var out []grantRow
	for rows.Next() {
		var g grantRow
		if err := rows.Scan(&g.amount, &g.drawn, &g.key, &g.expiresAt); err != nil {
			t.Fatalf("scan grant: %v", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read grants: %v", err)
	}
	return out
}

// TestGrantFundsThePoolAndLapsesAtReadAndSpendTime pins the grant life cycle.
// A grant funds the balance, a settle draws from it, and the store judges
// expiry at read and spend time, so lapsed credit is gone without a restart.
func TestGrantFundsThePoolAndLapsesAtReadAndSpendTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	clock := &testClock{at: base}
	store := mustCreditStore(t, path, clock)
	ctx := t.Context()
	mustGrant(t, store, 1000, base.Add(time.Hour), "")

	if got := mustBalance(t, store); got != 1000 {
		t.Fatalf("balance while the grant is live = %d, want 1000", got)
	}
	hold, err := store.Reserve(ctx, 400)
	if err != nil {
		t.Fatalf("reserve against the granted pool: %v", err)
	}
	if remaining, err := store.Remaining(ctx); err != nil || remaining != 600 {
		t.Fatalf("remaining while held = %d, %v, want 600", remaining, err)
	}
	if err := store.Settle(ctx, hold, 400); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if got := mustBalance(t, store); got != 600 {
		t.Fatalf("balance after the settle = %d, want 600", got)
	}

	clock.advance(2 * time.Hour)
	if got := mustBalance(t, store); got != 0 {
		t.Fatalf("balance after the grant lapsed = %d, want 0", got)
	}
	if _, err := store.Reserve(ctx, 1); !errors.Is(err, cost.ErrOverBudget) {
		t.Fatalf("reserve against lapsed credit error = %v, want ErrOverBudget", err)
	}
	rows := readGrants(t, path)
	if len(rows) != 1 {
		t.Fatalf("grant rows = %d, want 1, a grant is never deleted", len(rows))
	}
	if rows[0].drawn != 400 {
		t.Fatalf("drawn on the lapsed grant = %d, want 400", rows[0].drawn)
	}
}

// TestLapsedCreditCannotSurviveARestart closes the file, moves the clock past
// the expiry, and reopens. The balance a fresh store reports is computed from
// the stored expiry, not from anything the dead process remembered.
func TestLapsedCreditCannotSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	clock := &testClock{at: base}
	store, db := openAt(t, path,
		withLimit(1<<40), withClock(clock.now), withDenomination(credit))
	mustGrant(t, store, 1000, base.Add(time.Hour), "")
	if err := db.Close(); err != nil {
		t.Fatalf("close the database: %v", err)
	}

	clock.advance(2 * time.Hour)
	reopened := mustCreditStore(t, path, clock)
	if got := mustBalance(t, reopened); got != 0 {
		t.Fatalf("balance after the restart = %d, want 0", got)
	}
	if _, err := reopened.Reserve(t.Context(), 1); !errors.Is(err, cost.ErrOverBudget) {
		t.Fatalf("reserve after the restart error = %v, want ErrOverBudget", err)
	}
}

// TestSpendDrawsTheSoonestExpiringGrantFirst pins the drawdown order. Spend
// consumes the grant that expires soonest, so a mid-life expiry leaves the
// longer grant's remainder and never a negative balance.
func TestSpendDrawsTheSoonestExpiringGrantFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	clock := &testClock{at: base}
	store := mustCreditStore(t, path, clock)
	ctx := t.Context()
	mustGrant(t, store, 100, base.Add(time.Hour), "short")
	mustGrant(t, store, 100, base.Add(2*time.Hour), "long")

	hold, err := store.Reserve(ctx, 150)
	if err != nil {
		t.Fatalf("reserve 150 of 200: %v", err)
	}
	if err := store.Settle(ctx, hold, 150); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if got := mustBalance(t, store); got != 50 {
		t.Fatalf("balance after the settle = %d, want 50", got)
	}
	rows := readGrants(t, path)
	if rows[0].key != "short" || rows[0].drawn != 100 {
		t.Fatalf("soonest grant = %+v, want short drawn 100", rows[0])
	}
	if rows[1].key != "long" || rows[1].drawn != 50 {
		t.Fatalf("longer grant = %+v, want long drawn 50", rows[1])
	}

	// The short grant lapses with nothing undrawn left on it, so the live
	// balance is the long grant's remainder and not a negative sum.
	clock.advance(90 * time.Minute)
	if got := mustBalance(t, store); got != 50 {
		t.Fatalf("balance after the short grant lapsed = %d, want 50", got)
	}
	clock.advance(time.Hour)
	if got := mustBalance(t, store); got != 0 {
		t.Fatalf("balance after every grant lapsed = %d, want 0", got)
	}
	if _, err := store.Reserve(ctx, 1); !errors.Is(err, cost.ErrOverBudget) {
		t.Fatalf("reserve against lapsed credit error = %v, want ErrOverBudget", err)
	}
}

// TestSettleAfterExpiryDrawsNothing pins that a settle booked after its grant
// lapsed books the spend and draws nothing, so a correction cannot pull
// lapsed credit back into the pool.
func TestSettleAfterExpiryDrawsNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	clock := &testClock{at: base}
	store := mustCreditStore(t, path, clock)
	ctx := t.Context()
	mustGrant(t, store, 500, base.Add(time.Hour), "")
	hold, err := store.Reserve(ctx, 100)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := store.Settle(ctx, hold, 100); err != nil {
		t.Fatalf("settle: %v", err)
	}
	clock.advance(2 * time.Hour)
	if err := store.Settle(ctx, sqlitestore.Reservation{}, 100); err != nil {
		t.Fatalf("settle after expiry: %v", err)
	}
	if got := mustBalance(t, store); got != 0 {
		t.Fatalf("balance after the late settle = %d, want 0", got)
	}
	if rows := readGrants(t, path); rows[0].drawn != 100 {
		t.Fatalf("drawn after the late settle = %d, want 100", rows[0].drawn)
	}
}

// TestANegativeSettleReturnsCreditToTheSoonestGrant pins the correction path.
// A negative settle un-draws the soonest expiring grant first and never grows
// one past what was posted.
func TestANegativeSettleReturnsCreditToTheSoonestGrant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	clock := &testClock{at: base}
	store := mustCreditStore(t, path, clock)
	ctx := t.Context()
	mustGrant(t, store, 100, base.Add(time.Hour), "short")
	mustGrant(t, store, 100, base.Add(2*time.Hour), "long")
	hold, err := store.Reserve(ctx, 150)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := store.Settle(ctx, hold, 150); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := store.Settle(ctx, sqlitestore.Reservation{}, -30); err != nil {
		t.Fatalf("settle the correction: %v", err)
	}
	if got := mustBalance(t, store); got != 80 {
		t.Fatalf("balance after the correction = %d, want 80", got)
	}
	rows := readGrants(t, path)
	if rows[0].drawn != 70 {
		t.Fatalf("drawn on the corrected soonest grant = %d, want 70", rows[0].drawn)
	}
	if rows[1].drawn != 50 {
		t.Fatalf("drawn on the untouched grant = %d, want 50", rows[1].drawn)
	}
}

// TestGrantKeyPostsOnce pins the idempotent posting the monthly allowance
// rests on. A keyed grant posts once for good, a repeat changes nothing, and
// the guarantee survives a restart, while an empty key never repeats.
func TestGrantKeyPostsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	clock := &testClock{at: base}
	store, db := openAt(t, path,
		withLimit(1<<40), withClock(clock.now), withDenomination(credit))
	mustGrant(t, store, 1000, base.Add(30*24*time.Hour), "2024-03-01")
	if err := store.Grant(t.Context(), 1000, base.Add(30*24*time.Hour), "2024-03-01"); !errors.Is(err, sqlitestore.ErrGrantRepeated) {
		t.Fatalf("repeat grant error = %v, want ErrGrantRepeated", err)
	}
	if got := mustBalance(t, store); got != 1000 {
		t.Fatalf("balance after the repeat = %d, want 1000", got)
	}
	mustGrant(t, store, 1000, base.Add(61*24*time.Hour), "2024-04-01")
	mustGrant(t, store, 500, base.Add(time.Hour), "")
	mustGrant(t, store, 250, base.Add(2*time.Hour), "")
	if got := mustBalance(t, store); got != 2750 {
		t.Fatalf("balance = %d, want 2750", got)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the database: %v", err)
	}

	reopened := mustCreditStore(t, path, clock)
	if err := reopened.Grant(t.Context(), 1000, base.Add(30*24*time.Hour), "2024-03-01"); !errors.Is(err, sqlitestore.ErrGrantRepeated) {
		t.Fatalf("repeat grant after the restart error = %v, want ErrGrantRepeated", err)
	}
	if got := mustBalance(t, reopened); got != 2750 {
		t.Fatalf("balance after the restart = %d, want 2750", got)
	}
}

// TestGrantRefusesANegativeAmount pins the refusal, because a grant adds
// credit and a correction travels through the ledger as a charge.
func TestGrantRefusesANegativeAmount(t *testing.T) {
	clock := &testClock{at: base}
	store := mustCreditStore(t, filepath.Join(t.TempDir(), "cost.db"), clock)
	if err := store.Grant(t.Context(), -1, base.Add(time.Hour), ""); !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Fatalf("negative grant error = %v, want ErrInvalid", err)
	}
	if got := mustBalance(t, store); got != 0 {
		t.Fatalf("balance after the refused grant = %d, want 0", got)
	}
}

// TestChargesRefuseAForeignDenomination pins the refusal on both sides. A USD
// store refuses a credit charge and a credit store refuses a USD charge, and
// neither records anything, because no path converts one into the other.
func TestChargesRefuseAForeignDenomination(t *testing.T) {
	usdPath := filepath.Join(t.TempDir(), "usd.db")
	usd := openStore(t, usdPath, withLimit(1000))
	creditCharge := cost.Charge{Kind: "check", Units: 15, UnitPrice: 1, Denomination: credit}
	if err := usd.Add(t.Context(), creditCharge); !errors.Is(err, cost.ErrDenominationMismatch) {
		t.Fatalf("credit charge into a USD store error = %v, want ErrDenominationMismatch", err)
	}
	if charges, err := usd.Charges(t.Context()); err != nil || len(charges) != 0 {
		t.Fatalf("charges after the refusal = %v (err %v), want none", charges, err)
	}

	creditPath := filepath.Join(t.TempDir(), "credit.db")
	clock := &testClock{at: base}
	granted := mustCreditStore(t, creditPath, clock)
	usdCharge := cost.Charge{Kind: "gen", Units: 1, UnitPrice: cost.Cent}
	if err := granted.Add(t.Context(), usdCharge); !errors.Is(err, cost.ErrDenominationMismatch) {
		t.Fatalf("USD charge into a credit store error = %v, want ErrDenominationMismatch", err)
	}
	if charges, err := granted.Charges(t.Context()); err != nil || len(charges) != 0 {
		t.Fatalf("charges after the refusal = %v (err %v), want none", charges, err)
	}
	if got, err := granted.Total(t.Context()); err != nil || got != 0 {
		t.Fatalf("total in the credit store = %d, %v, want 0", got, err)
	}
}

// TestOpenRefusesADenominationChange pins that a store cannot reopen a file
// under another denomination, because every recorded amount would silently
// change meaning.
func TestOpenRefusesADenominationChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	clock := &testClock{at: base}
	store := mustCreditStore(t, path, clock)
	mustGrant(t, store, 100, base.Add(time.Hour), "")

	db := openDB(t, path)
	_, err := sqlitestore.Open(t.Context(), sqlitestore.Config{
		DB: db, Limit: 1 << 40, Now: clock.now, Logger: slog.New(slog.DiscardHandler),
	})
	if !errors.Is(err, cost.ErrDenominationMismatch) {
		t.Fatalf("reopen as USD error = %v, want ErrDenominationMismatch", err)
	}

	same := mustCreditStore(t, path, clock)
	if got := mustBalance(t, same); got != 100 {
		t.Fatalf("balance after the refused reopen = %d, want 100", got)
	}
}

// TestStoreWithoutGrantsKeepsTheCeilingAlone pins the default. A store that
// was never granted credit reports a zero balance and draws on its ceiling
// alone, exactly as every store did before grants existed.
func TestStoreWithoutGrantsKeepsTheCeilingAlone(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(1000))
	ctx := t.Context()
	if got := mustBalance(t, store); got != 0 {
		t.Fatalf("balance with no grants = %d, want 0", got)
	}
	if remaining, err := store.Remaining(ctx); err != nil || remaining != 1000 {
		t.Fatalf("remaining with no grants = %d, %v, want 1000", remaining, err)
	}
	hold, err := store.Reserve(ctx, 1000)
	if err != nil {
		t.Fatalf("reserve the whole ceiling with no grants: %v", err)
	}
	if err := store.Settle(ctx, hold, 1000); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if got := mustBalance(t, store); got != 0 {
		t.Fatalf("balance after the settle = %d, want 0", got)
	}
	if remaining, err := store.Remaining(ctx); err != nil || remaining != 0 {
		t.Fatalf("remaining after the settle = %d, %v, want 0", remaining, err)
	}
}

// TestKeyedBudgetDividesAGrantedPool pins the judge-session shape. Owner
// ceilings divide the live granted pool, an owner's spend never touches
// another's share, and the exhausted pool refuses every owner until the next
// grant lands.
func TestKeyedBudgetDividesAGrantedPool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	clock := &testClock{at: base}
	store := mustCreditStore(t, path, clock)
	ctx := t.Context()
	mustGrant(t, store, 1000, base.Add(30*24*time.Hour), "2024-03-01")
	keyed := sqlitestore.NewKeyedBudget(store)
	mustOwnerCeiling(t, keyed, "a", 300)
	mustOwnerCeiling(t, keyed, "b", 800)

	mustSettleKeyed(t, keyed, "a", 300)
	mustSettleKeyed(t, keyed, "b", 700)
	if got := mustBalance(t, store); got != 0 {
		t.Fatalf("balance after both owners settled = %d, want 0", got)
	}
	if _, err := keyed.Reserve(ctx, "b", 100); !errors.Is(err, cost.ErrOverBudget) {
		t.Fatalf("reserve past the exhausted pool error = %v, want ErrOverBudget", err)
	}
	if _, err := keyed.Reserve(ctx, "a", 100); !errors.Is(err, cost.ErrOverBudget) {
		t.Fatalf("reserve past the owner ceiling error = %v, want ErrOverBudget", err)
	}

	mustGrant(t, store, 500, base.Add(61*24*time.Hour), "2024-04-01")
	if _, err := keyed.Reserve(ctx, "b", 200); !errors.Is(err, cost.ErrOverBudget) {
		t.Fatalf("reserve past the owner ceiling after the refill error = %v, want ErrOverBudget", err)
	}
	hold, err := keyed.Reserve(ctx, "b", 100)
	if err != nil {
		t.Fatalf("reserve the refilled pool: %v", err)
	}
	if err := keyed.Settle(ctx, "b", hold, 100); err != nil {
		t.Fatalf("settle b: %v", err)
	}
	if got, err := keyed.Remaining(ctx, "b"); err != nil || got != 0 {
		t.Fatalf("remaining for b = %d, %v, want 0", got, err)
	}
	if got := mustBalance(t, store); got != 400 {
		t.Fatalf("balance after b settled = %d, want 400", got)
	}
}

// TestAMeterOverACreditPoolRefusesAUSDSink pins the sink carrying the
// denomination. A meter over a credit budget stamps its charge with the
// credit, so a USD store refuses to record it and the meter reports
// ErrUnrecordedCharge carrying that refusal.
func TestAMeterOverACreditPoolRefusesAUSDSink(t *testing.T) {
	budget, err := cost.NewBudgetIn(credit, 1000)
	if err != nil {
		t.Fatalf("NewBudgetIn: %v", err)
	}
	store := openStore(t, filepath.Join(t.TempDir(), "cost.db"), withLimit(1000))
	meter, err := cost.NewMeter(budget, store)
	if err != nil {
		t.Fatalf("NewMeter: %v", err)
	}
	usage, err := meter.Call(t.Context(), 20, "check", "job-9",
		func(context.Context) (cost.Usage, error) {
			return cost.Usage{Price: 15, Measured: true}, nil
		})
	if !errors.Is(err, cost.ErrUnrecordedCharge) {
		t.Fatalf("Call error = %v, want ErrUnrecordedCharge", err)
	}
	if !errors.Is(err, cost.ErrDenominationMismatch) {
		t.Errorf("Call error = %v, want it to wrap ErrDenominationMismatch", err)
	}
	if usage != (cost.Usage{}) {
		t.Errorf("Call usage = %+v, want the zero usage", usage)
	}
	if got := budget.Spent(); got != 15 {
		t.Errorf("Spent() = %d, want 15, the call happened and its spend is booked", got)
	}
	if charges, err := store.Charges(t.Context()); err != nil || len(charges) != 0 {
		t.Fatalf("charges = %v (err %v), want none, the store refused the foreign charge", charges, err)
	}
}

// TestAMeterOverACreditPoolRecordsACreditCharge pins the honest path. A meter
// over a credit budget driving a credit store lands a charge that names the
// credit, and the store's ledger and totals stay in that unit.
func TestAMeterOverACreditPoolRecordsACreditCharge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	clock := &testClock{at: base}
	store := mustCreditStore(t, path, clock)
	budget, err := cost.NewBudgetIn(credit, 1000)
	if err != nil {
		t.Fatalf("NewBudgetIn: %v", err)
	}
	meter, err := cost.NewMeter(budget, store)
	if err != nil {
		t.Fatalf("NewMeter: %v", err)
	}
	if _, err := meter.Call(t.Context(), 20, "check", "job-9",
		func(context.Context) (cost.Usage, error) {
			return cost.Usage{Price: 15, Measured: true}, nil
		}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	charges, err := store.Charges(t.Context())
	if err != nil {
		t.Fatalf("charges: %v", err)
	}
	if len(charges) != 1 {
		t.Fatalf("charges = %d rows, want 1", len(charges))
	}
	if got := charges[0].Denomination; got != credit {
		t.Errorf("recorded denomination = %+v, want %+v", got, credit)
	}
	if got, err := store.Total(t.Context()); err != nil || got != 15 {
		t.Errorf("total = %d, %v, want 15", got, err)
	}
	if got, err := store.TotalByKind(t.Context(), "check"); err != nil || got != 15 {
		t.Errorf("total by kind = %d, %v, want 15", got, err)
	}
}

// previousCostSchema is the schema the first three cost migration files
// wrote, before denominations and grants existed.
const previousCostSchema = `
CREATE TABLE cost_schema_migrations (
	filename   TEXT PRIMARY KEY,
	applied_at INTEGER NOT NULL
);
CREATE TABLE cost_charge (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    kind       TEXT    NOT NULL,
    units      INTEGER NOT NULL,
    unit_price INTEGER NOT NULL,
    ref        TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);
CREATE INDEX cost_charge_kind ON cost_charge (kind);
CREATE INDEX cost_charge_ref ON cost_charge (ref);
CREATE TABLE cost_budget (
    id       INTEGER PRIMARY KEY CHECK (id = 1),
    limit_nd INTEGER NOT NULL,
    spent_nd INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE cost_reservation (
    id         TEXT    PRIMARY KEY,
    amount_nd  INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX cost_reservation_expires ON cost_reservation (expires_at);
CREATE TABLE cost_owner_budget (
    owner    TEXT    PRIMARY KEY,
    limit_nd INTEGER NOT NULL,
    spent_nd INTEGER NOT NULL DEFAULT 0
);
ALTER TABLE cost_reservation ADD COLUMN owner TEXT NOT NULL DEFAULT '';
CREATE INDEX cost_reservation_owner ON cost_reservation (owner);
CREATE TABLE cost_settle (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    owner      TEXT    NOT NULL DEFAULT '',
    amount_nd  INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX cost_settle_created ON cost_settle (created_at);
CREATE INDEX cost_settle_owner_created ON cost_settle (owner, created_at);
`

// TestOpenMigratesAStoreFromThePreviousSchema hand-builds a database in the
// shape the earlier schema files left behind, records those files in the
// migration ledger, and opens the store over it. The upgrade must add the
// denomination columns and the grant table without touching the recorded
// spend, and the charges written before the upgrade must read back as USD.
func TestOpenMigratesAStoreFromThePreviousSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	seed := openFresh(t, path)
	if _, err := seed.Exec(previousCostSchema); err != nil {
		t.Fatalf("build the previous schema: %v", err)
	}
	for _, name := range []string{"0001_cost.sql", "0002_cost_owner.sql", "0003_cost_settle.sql"} {
		if _, err := seed.Exec(
			`INSERT INTO cost_schema_migrations (filename, applied_at) VALUES (?, 0)`, name); err != nil {
			t.Fatalf("record %s: %v", name, err)
		}
	}
	if _, err := seed.Exec(
		`INSERT INTO cost_charge (kind, units, unit_price, ref, created_at) VALUES ('gen', 3, 10, 'job-1', 0)`); err != nil {
		t.Fatalf("seed a charge: %v", err)
	}
	if _, err := seed.Exec(
		`INSERT INTO cost_budget (id, limit_nd, spent_nd) VALUES (1, 1000, 30)`); err != nil {
		t.Fatalf("seed the ceiling: %v", err)
	}

	store := openStore(t, path, withLimit(1000))
	ctx := t.Context()
	charges, err := store.Charges(ctx)
	if err != nil {
		t.Fatalf("charges: %v", err)
	}
	if len(charges) != 1 {
		t.Fatalf("charges = %d rows, want 1", len(charges))
	}
	if want := (cost.Charge{Kind: "gen", Units: 3, UnitPrice: 10, Ref: "job-1"}); charges[0] != want {
		t.Fatalf("migrated charge = %+v, want %+v with the zero denomination", charges[0], want)
	}
	if spent, err := store.Spent(ctx); err != nil || spent != 30 {
		t.Fatalf("spent after the upgrade = %d, %v, want 30", spent, err)
	}
	if err := store.Add(ctx, cost.Charge{Kind: "gen", Units: 1, UnitPrice: 5, Ref: "job-2"}); err != nil {
		t.Fatalf("add a charge after the upgrade: %v", err)
	}
	if n := freshCount(t, path, "cost_schema_migrations"); n != 6 {
		t.Fatalf("migration ledger rows = %d, want 6", n)
	}
	if n := freshCount(t, path, "cost_grant"); n != 0 {
		t.Fatalf("grant rows = %d, want an empty grant table", n)
	}
}
