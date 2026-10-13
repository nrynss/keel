package sqlitestore_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/cost/sqlitestore"
)

// withNamespace sets the schema namespace the store owns.
func withNamespace(namespace string) func(*sqlitestore.Config) {
	return func(c *sqlitestore.Config) { c.Namespace = namespace }
}

// creditUnit is the second denomination the namespace tests keep beside the
// default one. A store speaks one denomination for its whole life, so two
// units mean two stores, and the namespace is what lets both stores share
// one file.
var creditUnit = cost.Denomination{Name: "credit", Minor: 1}

// TestTwoNamespacesShareOneFileAsTwoStores reproduces the issue. Two
// stores, one per denomination, open on one database file under two
// namespaces. Each runs its own migrations, and every row one store writes
// stays invisible to the other.
func TestTwoNamespacesShareOneFileAsTwoStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	ctx := t.Context()
	usd := openStore(t, path, withNamespace("usd"), withLimit(cost.Dollar))
	pool := openStore(t, path, withNamespace("credits"),
		withLimit(1000), withDenomination(creditUnit))

	// A charge in one namespace is invisible to the other.
	charge := cost.Charge{Kind: "gen", Units: 1, UnitPrice: 500, Ref: "job-1"}
	if err := usd.Add(ctx, charge); err != nil {
		t.Fatalf("add to the usd store: %v", err)
	}
	if got, err := pool.Charges(ctx); err != nil || len(got) != 0 {
		t.Fatalf("credits store saw %d charges (err %v), want none", len(got), err)
	}
	if total, err := pool.Total(ctx); err != nil || total != 0 {
		t.Fatalf("credits store total = %d (err %v), want 0", total, err)
	}

	// A hold in one namespace leaves the other's headroom alone.
	hold, err := usd.Reserve(ctx, 300)
	if err != nil {
		t.Fatalf("reserve in the usd store: %v", err)
	}
	if remaining, err := pool.Remaining(ctx); err != nil || remaining != 1000 {
		t.Fatalf("credits remaining = %d (err %v), want 1000", remaining, err)
	}

	// A grant in one namespace funds no other.
	if err := pool.Grant(ctx, 400, time.Now().Add(time.Hour), "w1"); err != nil {
		t.Fatalf("grant in the credits store: %v", err)
	}
	if balance, err := pool.Balance(ctx); err != nil || balance != 400 {
		t.Fatalf("credits balance = %d (err %v), want 400", balance, err)
	}
	if balance, err := usd.Balance(ctx); err != nil || balance != 0 {
		t.Fatalf("usd balance = %d (err %v), want 0", balance, err)
	}

	// The same kind and reference pair books once per namespace, so a
	// settle in one store never stops the other from booking its own.
	booked, err := usd.SettleOnce(ctx, hold, 200, "gen", "job-1")
	if err != nil || !booked {
		t.Fatalf("settle once in the usd store = %v, %v, want true", booked, err)
	}
	booked, err = pool.SettleOnce(ctx, sqlitestore.Reservation{}, 50, "gen", "job-1")
	if err != nil || !booked {
		t.Fatalf("settle once in the credits store = %v, %v, want true", booked, err)
	}
	if spent, err := usd.Spent(ctx); err != nil || spent != 200 {
		t.Fatalf("usd spent = %d (err %v), want 200", spent, err)
	}
	if spent, err := pool.Spent(ctx); err != nil || spent != 50 {
		t.Fatalf("credits spent = %d (err %v), want 50", spent, err)
	}
	if n := freshCount(t, path, "usd_settle"); n != 1 {
		t.Fatalf("usd settle rows = %d, want 1", n)
	}
	if n := freshCount(t, path, "credits_settle"); n != 1 {
		t.Fatalf("credits settle rows = %d, want 1", n)
	}

	// A quote claims, runs and books inside its own namespace, so a run
	// never touches another store's quote, settle or ledger rows.
	if err := sqlitestore.NewKeyedBudget(pool).SetLimit(ctx, "app", 500); err != nil {
		t.Fatalf("set the owner ceiling in the credits store: %v", err)
	}
	quote, err := pool.Quote(ctx, "app", 100, time.Minute)
	if err != nil {
		t.Fatalf("quote in the credits store: %v", err)
	}
	usage, err := pool.Run(ctx, quote.ID, func(ctx context.Context) (cost.Usage, error) {
		return cost.Usage{}, nil
	})
	if err != nil || usage.Price != 100 {
		t.Fatalf("run the quote = %+v, %v, want a 100 settle", usage, err)
	}
	if spent, err := pool.Spent(ctx); err != nil || spent != 150 {
		t.Fatalf("credits spent after the run = %d (err %v), want 150", spent, err)
	}
	if spent, err := usd.Spent(ctx); err != nil || spent != 200 {
		t.Fatalf("usd spent after the run = %d (err %v), want 200", spent, err)
	}
	if n := freshCount(t, path, "credits_quote"); n != 1 {
		t.Fatalf("credits quote rows = %d, want the one done quote", n)
	}
	if n := freshCount(t, path, "usd_quote"); n != 0 {
		t.Fatalf("usd quote rows = %d, want none", n)
	}

	// Each namespace ran its own migrations and keeps its own ledger.
	for _, namespace := range []string{"usd", "credits"} {
		if n := freshCount(t, path, namespace+"_schema_migrations"); n != 6 {
			t.Fatalf("%s migration ledger rows = %d, want 6", namespace, n)
		}
	}
}

// TestDefaultNamespaceKeepsTheHistoricalNames writes a charge through the
// empty namespace, closes the handle, reopens the file, and reads the
// charge back. The empty namespace keeps the table names every earlier
// version wrote, so a default store never migrates.
func TestDefaultNamespaceKeepsTheHistoricalNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	store, db := openAt(t, path)
	ctx := t.Context()
	charge := cost.Charge{Kind: "gen", Units: 3, UnitPrice: 10, Ref: "job-1"}
	if err := store.Add(ctx, charge); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the handle: %v", err)
	}

	reopened := openStore(t, path)
	got, err := reopened.Charges(ctx)
	if err != nil {
		t.Fatalf("charges after the reopen: %v", err)
	}
	if len(got) != 1 || got[0] != charge {
		t.Fatalf("charges after the reopen = %+v, want %+v", got, []cost.Charge{charge})
	}
	if n := freshCount(t, path, "cost_schema_migrations"); n != 6 {
		t.Fatalf("migration ledger rows = %d, want 6", n)
	}
	if n := freshCount(t, path, "cost_charge"); n != 1 {
		t.Fatalf("charge rows = %d, want 1", n)
	}
}

// fixedNameSchema is the schema the package built before the namespace
// token existed: the ledger rows of all six files, followed by the same
// SQL with the fixed cost_ prefix in every table and index name. The
// rewritten default namespace produces those very names, so a
// default-namespace store must open a file in this shape and read its rows
// unchanged.
const fixedNameSchema = `
CREATE TABLE cost_schema_migrations (
	filename   TEXT PRIMARY KEY,
	applied_at INTEGER NOT NULL
);
INSERT INTO cost_schema_migrations (filename, applied_at) VALUES
	('0001_cost.sql', 0),
	('0002_cost_owner.sql', 0),
	('0003_cost_settle.sql', 0),
	('0004_cost_denomination.sql', 0),
	('0005_cost_quote.sql', 0),
	('0006_cost_settle_once.sql', 0);
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
ALTER TABLE cost_charge ADD COLUMN denomination TEXT NOT NULL DEFAULT '';
ALTER TABLE cost_budget ADD COLUMN denomination TEXT NOT NULL DEFAULT '';
ALTER TABLE cost_budget ADD COLUMN minor_units INTEGER NOT NULL DEFAULT 0;
CREATE TABLE cost_grant (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    amount_nd  INTEGER NOT NULL,
    drawn_nd   INTEGER NOT NULL DEFAULT 0,
    grant_key  TEXT    NOT NULL DEFAULT '',
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX cost_grant_key ON cost_grant (grant_key) WHERE grant_key <> '';
CREATE INDEX cost_grant_expires ON cost_grant (expires_at);
CREATE TABLE cost_quote (
    id               TEXT    PRIMARY KEY,
    owner            TEXT    NOT NULL,
    price_nd         INTEGER NOT NULL,
    denomination     TEXT    NOT NULL DEFAULT '',
    state            TEXT    NOT NULL DEFAULT 'open',
    outcome_nd       INTEGER NOT NULL DEFAULT 0,
    outcome_measured INTEGER NOT NULL DEFAULT 0,
    reservation_id   TEXT    NOT NULL DEFAULT '',
    claim_expires_at INTEGER NOT NULL DEFAULT 0,
    expires_at       INTEGER NOT NULL,
    created_at       INTEGER NOT NULL
);
CREATE INDEX cost_quote_expires ON cost_quote (expires_at);
CREATE INDEX cost_quote_owner ON cost_quote (owner);
CREATE INDEX cost_quote_claim ON cost_quote (claim_expires_at) WHERE state = 'claimed';
CREATE TABLE cost_settle_once (
    kind       TEXT    NOT NULL,
    ref        TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (kind, ref)
);
`

// TestDefaultNamespaceReadsAFixedNameSchema hand-builds a database in the
// shape the fixed-name migrations left behind and opens a default-namespace
// store over it. The store must find its tables under the historical names,
// read the charge that was written there, and run no migration again.
func TestDefaultNamespaceReadsAFixedNameSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	seed := openFresh(t, path)
	if _, err := seed.Exec(fixedNameSchema); err != nil {
		t.Fatalf("build the fixed-name schema: %v", err)
	}
	if _, err := seed.Exec(
		`INSERT INTO cost_charge (kind, units, unit_price, ref, denomination, created_at)
			VALUES ('gen', 3, 10, 'job-1', '', 0)`); err != nil {
		t.Fatalf("seed a charge: %v", err)
	}
	if _, err := seed.Exec(
		`INSERT INTO cost_budget (id, limit_nd, spent_nd, denomination, minor_units)
			VALUES (1, 1000, 30, '', 0)`); err != nil {
		t.Fatalf("seed the ceiling: %v", err)
	}

	store := openStore(t, path, withLimit(1000))
	ctx := t.Context()
	charges, err := store.Charges(ctx)
	if err != nil {
		t.Fatalf("charges over the fixed-name schema: %v", err)
	}
	want := []cost.Charge{{Kind: "gen", Units: 3, UnitPrice: 10, Ref: "job-1"}}
	if len(charges) != 1 || charges[0] != want[0] {
		t.Fatalf("charges over the fixed-name schema = %+v, want %+v", charges, want)
	}
	if spent, err := store.Spent(ctx); err != nil || spent != 30 {
		t.Fatalf("spent over the fixed-name schema = %d, %v, want 30", spent, err)
	}
	if err := store.Add(ctx, cost.Charge{Kind: "gen", Units: 1, UnitPrice: 5, Ref: "job-2"}); err != nil {
		t.Fatalf("add a charge over the fixed-name schema: %v", err)
	}
	if n := freshCount(t, path, "cost_schema_migrations"); n != 6 {
		t.Fatalf("migration ledger rows = %d, want 6", n)
	}
}

// TestNamespaceReadsAsLowercase opens a store as USD and again as usd. The
// two spellings are one namespace, so the second open succeeds and sees the
// first store's charges instead of failing or starting a second set of
// tables.
func TestNamespaceReadsAsLowercase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	ctx := t.Context()
	upper := openStore(t, path, withNamespace("USD"))
	charge := cost.Charge{Kind: "gen", Units: 1, UnitPrice: 5, Ref: "job-1"}
	if err := upper.Add(ctx, charge); err != nil {
		t.Fatalf("add: %v", err)
	}

	twin := openStore(t, path, withNamespace("usd"))
	got, err := twin.Charges(ctx)
	if err != nil {
		t.Fatalf("charges through the case twin: %v", err)
	}
	if len(got) != 1 || got[0] != charge {
		t.Fatalf("charges through the case twin = %+v, want %+v", got, []cost.Charge{charge})
	}
	if n := freshCount(t, path, "usd_schema_migrations"); n != 6 {
		t.Fatalf("migration ledger rows = %d, want 6", n)
	}
	if n := freshCount(t, path, "usd_charge"); n != 1 {
		t.Fatalf("charge rows through the case twin = %d, want 1", n)
	}
}
