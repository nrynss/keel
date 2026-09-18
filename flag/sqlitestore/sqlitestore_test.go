package sqlitestore_test

import (
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nrynss/keel/flag"
	"github.com/nrynss/keel/flag/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// base is the instant the tests stamp flag changes with. It carries a
// non-zero nanosecond part, so a store that lost precision would be caught.
var base = time.Date(2024, 3, 1, 12, 0, 0, 123456789, time.UTC)

// openDB opens the database file at path for a test and closes it at cleanup.
func openDB(t *testing.T, path string) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Config{
		Path:   path,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("open sqlite %q: %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() }) // the handle is discarded at cleanup, so a close failure cannot fail the test
	return db
}

// openStore applies the schema at path and returns the store.
func openStore(t *testing.T, path string) *sqlitestore.Store {
	t.Helper()
	cfg := sqlitestore.Config{
		DB:     openDB(t, path),
		Now:    func() time.Time { return base },
		Logger: slog.New(slog.DiscardHandler),
	}
	store, err := sqlitestore.Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("open store %q: %v", path, err)
	}
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

// TestFlipVisibleWithoutRestart pins the behaviour the package exists for. A
// flag set through one store connection reads back at its new value through
// a second connection opened afterwards, with no restart in between.
func TestFlipVisibleWithoutRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flag.db")
	paid := flag.Bool{Name: "paid_calls", Default: false, Help: "switch paid calls on"}

	first := openStore(t, path)
	ctx := t.Context()
	if _, _, err := first.Bool(ctx, paid); err != nil {
		t.Fatalf("read before flip: %v", err)
	}
	changed, err := first.SetBool(ctx, paid, true)
	if err != nil {
		t.Fatalf("set: %v", err)
	}

	second := openStore(t, path)
	got, at, err := second.Bool(ctx, paid)
	if err != nil {
		t.Fatalf("read after flip: %v", err)
	}
	if !got {
		t.Fatal("flag read false after a second connection set it true")
	}
	if !at.Equal(changed) {
		t.Fatalf("changed time = %v, want %v", at, changed)
	}
}

// TestUnknownReadsDeclaredDefault pins that a name with no row reads as its
// declared default, and that a stored false is distinct from that default.
func TestUnknownReadsDeclaredDefault(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "flag.db"))
	ctx := t.Context()
	kill := flag.Bool{Name: "kill_switch", Default: true, Help: "stop paid work"}

	got, at, err := store.Bool(ctx, kill)
	if err != nil {
		t.Fatalf("read unknown flag: %v", err)
	}
	if !got {
		t.Fatal("unknown flag read false, want the declared default true")
	}
	if !at.IsZero() {
		t.Fatalf("unknown flag changed time = %v, want the zero time", at)
	}

	// A stored false must be distinguishable from the missing row.
	if _, err := store.SetBool(ctx, kill, false); err != nil {
		t.Fatalf("set false: %v", err)
	}
	got, at, err = store.Bool(ctx, kill)
	if err != nil {
		t.Fatalf("read stored false: %v", err)
	}
	if got {
		t.Fatal("stored false read true")
	}
	if !at.Equal(base) {
		t.Fatalf("stored false changed time = %v, want %v", at, base)
	}

	// Setting the flag back to the default still counts as a stored value,
	// stamped as a change rather than as a missing row.
	if _, err := store.SetBool(ctx, kill, true); err != nil {
		t.Fatalf("set true: %v", err)
	}
	got, at, err = store.Bool(ctx, kill)
	if err != nil {
		t.Fatalf("read stored true: %v", err)
	}
	if !got || at.IsZero() {
		t.Fatalf("stored true = %v at %v, want true at a stamped time", got, at)
	}
}

// TestTextRoundTrip pins the small-value half. A text flag reads its default
// before anyone sets it, and its stored value afterwards.
func TestTextRoundTrip(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "flag.db"))
	ctx := t.Context()
	model := flag.Text{Name: "model", Default: "standard", Help: "which model tier serves calls"}

	got, at, err := store.Text(ctx, model)
	if err != nil {
		t.Fatalf("read unknown text flag: %v", err)
	}
	if got != "standard" || !at.IsZero() {
		t.Fatalf("unknown text flag = %q at %v, want the default at the zero time", got, at)
	}

	if _, err := store.SetText(ctx, model, "premium"); err != nil {
		t.Fatalf("set text: %v", err)
	}
	got, at, err = store.Text(ctx, model)
	if err != nil {
		t.Fatalf("read text flag: %v", err)
	}
	if got != "premium" || !at.Equal(base) {
		t.Fatalf("text flag = %q at %v, want premium at %v", got, at, base)
	}
}

// TestReadRefusesForeignKind pins that a row written under one kind never
// comes back from a read that declared the other kind.
func TestReadRefusesForeignKind(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "flag.db"))
	ctx := t.Context()
	text := flag.Text{Name: "shared", Default: "", Help: "declared as text"}
	if _, err := store.SetText(ctx, text, "words"); err != nil {
		t.Fatalf("set text: %v", err)
	}

	read := flag.Bool{Name: "shared", Default: false, Help: "read as bool"}
	if _, _, err := store.Bool(ctx, read); !errors.Is(err, flag.ErrKind) {
		t.Fatalf("bool read of a text row error = %v, want ErrKind", err)
	}

	// The same holds the other way round.
	if _, err := store.SetBool(ctx, read, true); err != nil {
		t.Fatalf("set bool: %v", err)
	}
	if _, _, err := store.Text(ctx, text); !errors.Is(err, flag.ErrKind) {
		t.Fatalf("text read of a bool row error = %v, want ErrKind", err)
	}
}

// TestOpenAppliesMigrationsOnce pins that the schema lands on a fresh
// database and that a second open runs nothing again.
func TestOpenAppliesMigrationsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flag.db")
	openStore(t, path)
	openStore(t, path)
	if n := freshCount(t, path, "flag_schema_migrations"); n != 1 {
		t.Fatalf("migration ledger rows = %d, want 1", n)
	}
}

// TestOpenRejectsNilDatabase pins the open refusal a caller must classify.
func TestOpenRejectsNilDatabase(t *testing.T) {
	ctx := t.Context()
	if _, err := sqlitestore.Open(ctx, sqlitestore.Config{}); !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Fatalf("open with no database error = %v, want ErrInvalid", err)
	}
}

// TestSetRejectsEmptyName pins the declaration refusal a caller must
// classify.
func TestSetRejectsEmptyName(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "flag.db"))
	ctx := t.Context()
	if _, err := store.SetBool(ctx, flag.Bool{}, true); !errors.Is(err, flag.ErrInvalid) {
		t.Fatalf("set bool with no name error = %v, want ErrInvalid", err)
	}
	if _, err := store.SetText(ctx, flag.Text{}, "x"); !errors.Is(err, flag.ErrInvalid) {
		t.Fatalf("set text with no name error = %v, want ErrInvalid", err)
	}
}
