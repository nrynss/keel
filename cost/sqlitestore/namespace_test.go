package sqlitestore_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/cost/sqlitestore"
	"github.com/nrynss/keel/sqlite"
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

// refusedOpen attempts an open that must fail with ErrInvalid, and returns
// the error for the caller to inspect.
func refusedOpen(t *testing.T, path, namespace string) error {
	t.Helper()
	_, err := sqlitestore.Open(t.Context(), sqlitestore.Config{
		DB:        openDB(t, path),
		Namespace: namespace,
		Limit:     cost.Dollar,
		Logger:    slog.New(slog.DiscardHandler),
	})
	if !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Fatalf("open %q error = %v, want ErrInvalid", namespace, err)
	}
	return err
}

// TestOpenRefusesANamespaceEndingInTheOwnerSuffix reproduces the issue. The
// budget table of usd_owner is usd_owner_budget, which is also the
// owner-budget table of usd. Open must refuse the second namespace before
// any migration runs, and the first store must keep working.
func TestOpenRefusesANamespaceEndingInTheOwnerSuffix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	usd := openStore(t, path, withNamespace("usd"), withLimit(cost.Dollar))

	err := refusedOpen(t, path, "usd_owner")
	if !strings.Contains(err.Error(), "usd_owner") {
		t.Fatalf("open usd_owner error = %v, want it to name the namespace", err)
	}

	// The refused open wrote nothing, so the usd store keeps working.
	ctx := t.Context()
	charge := cost.Charge{Kind: "gen", Units: 1, UnitPrice: 500, Ref: "job-1"}
	if err := usd.Add(ctx, charge); err != nil {
		t.Fatalf("add after the refused open: %v", err)
	}
	got, err := usd.Charges(ctx)
	if err != nil || len(got) != 1 || got[0] != charge {
		t.Fatalf("charges after the refused open = %+v (err %v), want %+v", got, err, []cost.Charge{charge})
	}
}

// TestOpenRefusesTheOwnerSuffixAloneOnAFreshFile opens the suffixed
// namespace first, with no other namespace on the file. The refusal is
// unconditional, because the base namespace may open later, and the file
// must stay usable for it afterwards.
func TestOpenRefusesTheOwnerSuffixAloneOnAFreshFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	refusedOpen(t, path, "usd_owner")
	// The namespace folds to lowercase before the check, so the upper
	// spelling is the same refusal.
	refusedOpen(t, path, "USD_OWNER")

	// The refusals wrote nothing, so the base namespace opens the file.
	store := openStore(t, path, withNamespace("usd"), withLimit(cost.Dollar))
	ctx := t.Context()
	charge := cost.Charge{Kind: "gen", Units: 2, UnitPrice: 10, Ref: "job-2"}
	if err := store.Add(ctx, charge); err != nil {
		t.Fatalf("add after the refused opens: %v", err)
	}
	got, err := store.Charges(ctx)
	if err != nil || len(got) != 1 || got[0] != charge {
		t.Fatalf("charges after the refused opens = %+v (err %v), want %+v", got, err, []cost.Charge{charge})
	}
}

// TestANamespaceContainingTheSuffixButNotEndingInItStillOpens pins the
// boundary of the refusal. The suffix must sit at the end of the name, so
// owner_pool opens and owns its tables beside any neighbour.
func TestANamespaceContainingTheSuffixButNotEndingInItStillOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cost.db")
	store := openStore(t, path, withNamespace("owner_pool"), withLimit(1000))
	ctx := t.Context()
	charge := cost.Charge{Kind: "gen", Units: 1, UnitPrice: 5, Ref: "job-1"}
	if err := store.Add(ctx, charge); err != nil {
		t.Fatalf("add: %v", err)
	}
	got, err := store.Charges(ctx)
	if err != nil || len(got) != 1 || got[0] != charge {
		t.Fatalf("charges = %+v (err %v), want %+v", got, err, []cost.Charge{charge})
	}
	if n := freshCount(t, path, "owner_pool_schema_migrations"); n != 6 {
		t.Fatalf("migration ledger rows = %d, want 6", n)
	}
}

// TestStrandedLedgerReopensAsAFreshStore hand-builds the file an earlier
// layout leaves behind under a custom namespace: the rows sit under the
// fixed default names while the ledger records every migration as applied
// for the qualified one. Open must read that ledger as vouching for tables
// it cannot see, apply the schema again, and hand back a fresh store. The
// rows under the old names stay on disk unread.
func TestStrandedLedgerReopensAsAFreshStore(t *testing.T) {
	const namespace = "credits"
	path := filepath.Join(t.TempDir(), "cost.db")
	seed := openFresh(t, path)
	schema := strings.ReplaceAll(fixedNameSchema,
		"cost_schema_migrations", namespace+"_schema_migrations")
	if _, err := seed.Exec(schema); err != nil {
		t.Fatalf("build the stranded schema: %v", err)
	}
	if _, err := seed.Exec(
		`INSERT INTO cost_charge (kind, units, unit_price, ref, denomination, created_at)
			VALUES ('gen', 3, 10, 'job-1', '', 0)`); err != nil {
		t.Fatalf("seed a charge under the old names: %v", err)
	}
	if _, err := seed.Exec(
		`INSERT INTO cost_budget (id, limit_nd, spent_nd, denomination, minor_units)
			VALUES (1, 1000, 30, '', 0)`); err != nil {
		t.Fatalf("seed the ceiling under the old names: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close the seed connection: %v", err)
	}

	store := openStore(t, path, withNamespace(namespace), withLimit(1000))
	ctx := t.Context()
	if got, err := store.Charges(ctx); err != nil || len(got) != 0 {
		t.Fatalf("charges over the reopened store = %+v (err %v), want an empty ledger", got, err)
	}
	if spent, err := store.Spent(ctx); err != nil || spent != 0 {
		t.Fatalf("spent over the reopened store = %d (err %v), want 0", spent, err)
	}
	if err := store.Add(ctx, cost.Charge{Kind: "gen", Units: 1, UnitPrice: 5, Ref: "fresh-1"}); err != nil {
		t.Fatalf("add a charge over the reopened store: %v", err)
	}
	if n := freshCount(t, path, namespace+"_charge"); n != 1 {
		t.Fatalf("qualified charge rows = %d, want the one fresh charge", n)
	}
	if n := freshCount(t, path, "cost_charge"); n != 1 {
		t.Fatalf("charge rows under the old name = %d, want the seeded row still on disk", n)
	}
	if spent := freshSpent(t, path, "cost_budget"); spent != 30 {
		t.Fatalf("spent under the old name = %d, want the seeded 30 still on disk", spent)
	}
	if n := freshCount(t, path, namespace+"_schema_migrations"); n != 6 {
		t.Fatalf("migration ledger rows after the reopen = %d, want 6", n)
	}
}

// TestOpenRefusesAPartialTableSet drops one table from a working namespace
// store and reopens. Every migration commits whole or not at all, so a
// schema one table short is corruption no ordinary path produces. The
// ledger stays complete, so the open must refuse the file with ErrInvalid
// instead of healing it.
func TestOpenRefusesAPartialTableSet(t *testing.T) {
	const namespace = "credits"
	path := filepath.Join(t.TempDir(), "cost.db")
	openStore(t, path, withNamespace(namespace), withLimit(1000))
	if _, err := openFresh(t, path).Exec(`DROP TABLE ` + namespace + `_grant`); err != nil {
		t.Fatalf("drop a qualified table: %v", err)
	}
	if n := freshCount(t, path, namespace+"_schema_migrations"); n != 6 {
		t.Fatalf("migration ledger rows = %d, want the complete six the refusal reads", n)
	}
	_, err := sqlitestore.Open(t.Context(), sqlitestore.Config{
		DB:        openDB(t, path),
		Limit:     1000,
		Namespace: namespace,
		Logger:    slog.New(slog.DiscardHandler),
	})
	if !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Fatalf("open over a one-table-short schema error = %v, want ErrInvalid", err)
	}
}

// TestOpenRefusesASupersetLedgerBesideAPartialTableSet drops one table from
// a working namespace store and records one ledger row for a file this
// binary does not carry, the shape a rollback from a newer binary leaves
// beside outside damage. The ledger still records every file this binary
// carries, so the open must refuse the file with ErrInvalid instead of
// opening it and failing raw on the first read of the dropped table.
func TestOpenRefusesASupersetLedgerBesideAPartialTableSet(t *testing.T) {
	const namespace = "credits"
	path := filepath.Join(t.TempDir(), "cost.db")
	openStore(t, path, withNamespace(namespace), withLimit(1000))
	seed := openFresh(t, path)
	if _, err := seed.Exec(`DROP TABLE ` + namespace + `_grant`); err != nil {
		t.Fatalf("drop a qualified table: %v", err)
	}
	if _, err := seed.Exec(
		`INSERT INTO ` + namespace + `_schema_migrations (filename, applied_at) VALUES ('0007_future.sql', 0)`); err != nil {
		t.Fatalf("record a file this binary does not carry: %v", err)
	}
	if n := freshCount(t, path, namespace+"_schema_migrations"); n != 7 {
		t.Fatalf("migration ledger rows = %d, want the superset seven", n)
	}
	if n := freshPresentTables(t, path, namespace); n != 7 {
		t.Fatalf("qualified tables present = %d, want the one-table-short seven", n)
	}
	_, err := sqlitestore.Open(t.Context(), sqlitestore.Config{
		DB:        openDB(t, path),
		Limit:     1000,
		Namespace: namespace,
		Logger:    slog.New(slog.DiscardHandler),
	})
	if !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Fatalf("open over a superset ledger beside a partial table set error = %v, want ErrInvalid", err)
	}
}

// TestLedgerTableMirrorsTheRunnerNaming pins the ledger table name the
// reset at open clears. The name mirrors the migration runner's own ledger
// naming, so a fresh namespace must leave one ledger row per migration file
// in a table of exactly that name. A silent rename in the runner then fails
// here instead of turning the reset into a delete on a table that is not
// there.
func TestLedgerTableMirrorsTheRunnerNaming(t *testing.T) {
	const namespace = "mirror"
	path := filepath.Join(t.TempDir(), "cost.db")
	openStore(t, path, withNamespace(namespace), withLimit(1000))
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatalf("list the migration files: %v", err)
	}
	want := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			want++
		}
	}
	if n := freshCount(t, path, namespace+"_schema_migrations"); n != want {
		t.Fatalf("migration ledger rows = %d, want one per migration file (%d)", n, want)
	}
}

// freshPresentTables counts the qualified tables a namespace owns, read over
// a fresh connection with every name bound as an argument. The suffixes
// list every table the migrations create.
func freshPresentTables(t *testing.T, path, namespace string) int {
	t.Helper()
	suffixes := []string{
		"charge", "budget", "reservation", "owner_budget",
		"settle", "settle_once", "grant", "quote",
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(suffixes)), ", ")
	args := make([]any, len(suffixes))
	for i, suffix := range suffixes {
		args[i] = namespace + "_" + suffix
	}
	var n int
	if err := openFresh(t, path).QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN (`+placeholders+`)`,
		args...).Scan(&n); err != nil {
		t.Fatalf("count the qualified tables: %v", err)
	}
	return n
}

// strandedFixtureFile hand-builds the file an earlier layout leaves behind
// under a custom namespace: the rows sit under the fixed default names
// while a full ledger records every migration as applied for the qualified
// prefix. The seed connection closes before the call returns, so the file
// is quiescent for the opens that follow.
func strandedFixtureFile(t *testing.T, path, namespace string) {
	t.Helper()
	seed := openFresh(t, path)
	schema := strings.ReplaceAll(fixedNameSchema,
		"cost_schema_migrations", namespace+"_schema_migrations")
	if _, err := seed.Exec(schema); err != nil {
		t.Fatalf("build the stranded schema: %v", err)
	}
	if _, err := seed.Exec(
		`INSERT INTO cost_charge (kind, units, unit_price, ref, denomination, created_at)
			VALUES ('gen', 3, 10, 'job-1', '', 0)`); err != nil {
		t.Fatalf("seed a charge under the old names: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close the seed connection: %v", err)
	}
}

// TestConcurrentOpensOfAStrandedFileConverge opens one stranded fixture
// from three handles at once, round after round. The sweep at open holds
// its guard and its erase in one statement, so no opener can erase the rows
// another opener's re-apply has already recorded. Every open must succeed
// or refuse with ErrInvalid, and every round must leave the file
// consistent, with a full ledger beside its eight tables and a canary open
// that succeeds.
func TestConcurrentOpensOfAStrandedFileConverge(t *testing.T) {
	const (
		namespace = "credits"
		rounds    = 8
		openers   = 3
	)
	ctx := t.Context()
	credit := cost.Denomination{Name: "credit", Minor: 1}
	for round := 0; round < rounds; round++ {
		path := filepath.Join(t.TempDir(), "cost.db")
		strandedFixtureFile(t, path, namespace)

		handles := make([]*sqlite.DB, openers)
		results := make([]error, openers)
		for i := range handles {
			handles[i] = openDB(t, path)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range handles {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				cfg := sqlitestore.Config{
					DB:           handles[i],
					Namespace:    namespace,
					Limit:        1000,
					Denomination: credit,
					Logger:       slog.New(slog.DiscardHandler),
				}
				_, results[i] = sqlitestore.Open(ctx, cfg)
			}(i)
		}
		close(start)
		wg.Wait()
		for i, err := range results {
			if err == nil || errors.Is(err, sqlitestore.ErrInvalid) {
				continue
			}
			t.Fatalf("round %d opener %d failed with an unclassified error: %v", round, i, err)
		}

		// The file must have converged whichever opener won the race. A
		// canary open succeeds, the ledger holds one row per migration
		// file, and all eight qualified tables exist beside it.
		canary := openStore(t, path, withNamespace(namespace), withLimit(1000), withDenomination(credit))
		if _, err := canary.Charges(ctx); err != nil {
			t.Fatalf("round %d canary read: %v", round, err)
		}
		if n := freshCount(t, path, namespace+"_schema_migrations"); n != 6 {
			t.Fatalf("round %d migration ledger rows = %d, want 6", round, n)
		}
		if n := freshPresentTables(t, path, namespace); n != 8 {
			t.Fatalf("round %d qualified tables present = %d, want 8", round, n)
		}
	}
}

// TestReopenFinishesAnInterruptedReapply hand-builds the state a crash mid
// re-apply leaves. The sweep has cleared the ledger, files 0001 through
// 0003 of the re-apply have committed, and the rest have not. The reopen
// must finish the remaining files instead of failing on the tables that
// already stand, and the rows the first three files recorded must survive
// untouched. Their hand-inserted stamps read zero, so a surviving stamp
// proves the reopen spared them rather than erasing and re-recording them.
func TestReopenFinishesAnInterruptedReapply(t *testing.T) {
	const namespace = "credits"
	path := filepath.Join(t.TempDir(), "cost.db")
	strandedFixtureFile(t, path, namespace)

	observer := openFresh(t, path)
	if _, err := observer.Exec(`DELETE FROM ` + namespace + `_schema_migrations`); err != nil {
		t.Fatalf("play the ledger sweep: %v", err)
	}
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatalf("list the migration files: %v", err)
	}
	for _, entry := range entries[:3] {
		body, err := os.ReadFile(filepath.Join("migrations", entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		rewritten := strings.ReplaceAll(string(body), "{{table}}", namespace)
		if _, err := observer.Exec(rewritten); err != nil {
			t.Fatalf("apply %s: %v", entry.Name(), err)
		}
		if _, err := observer.Exec(
			`INSERT INTO `+namespace+`_schema_migrations (filename, applied_at) VALUES (?, 0)`,
			entry.Name()); err != nil {
			t.Fatalf("record %s: %v", entry.Name(), err)
		}
	}
	if n := freshCount(t, path, namespace+"_schema_migrations"); n != 3 {
		t.Fatalf("mid-reapply ledger rows = %d, want 3", n)
	}
	if n := freshPresentTables(t, path, namespace); n != 5 {
		t.Fatalf("mid-reapply qualified tables present = %d, want the five of the first three files", n)
	}

	store := openStore(t, path, withNamespace(namespace), withLimit(1000))
	ctx := t.Context()
	if err := store.Add(ctx, cost.Charge{Kind: "gen", Units: 1, UnitPrice: 5, Ref: "fresh-1"}); err != nil {
		t.Fatalf("add over the converged store: %v", err)
	}
	if got, err := store.Charges(ctx); err != nil || len(got) != 1 {
		t.Fatalf("charges over the converged store = %+v (err %v), want the one fresh charge", got, err)
	}
	if n := freshCount(t, path, namespace+"_schema_migrations"); n != 6 {
		t.Fatalf("ledger rows after the reopen = %d, want 6", n)
	}
	if n := freshPresentTables(t, path, namespace); n != 8 {
		t.Fatalf("qualified tables present after the reopen = %d, want 8", n)
	}
	var spared int
	if err := openFresh(t, path).QueryRow(
		`SELECT COUNT(*) FROM ` + namespace + `_schema_migrations WHERE applied_at = 0`).Scan(&spared); err != nil {
		t.Fatalf("read the surviving stamps: %v", err)
	}
	if spared != 3 {
		t.Fatalf("rows still carrying their original stamp = %d, want the 3 the reopen had to spare", spared)
	}
}
