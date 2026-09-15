package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	moderncsqlite "modernc.org/sqlite"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

// openTestDB opens a database for a test and closes it at cleanup. Records go
// to a discarding logger so a passing test does not write to stderr.
func openTestDB(t *testing.T, cfg Config) *DB {
	t.Helper()
	cfg.Logger = slog.New(slog.DiscardHandler)
	db, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open(%+v): %v", cfg, err)
	}
	t.Cleanup(func() { _ = db.Close() }) // the handle is discarded here, so a close failure cannot fail the test
	return db
}

// openFresh opens the database file at path over a new connection, with no
// pragmas from this package. It is the independent observer the tests query.
func openFresh(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() }) // the handle is discarded here, so a close failure cannot fail the test
	return db
}

// mustExec runs a statement on pool and fails the test on error.
func mustExec(t *testing.T, pool *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// ledgerRows reads a namespace version table over a fresh connection and
// returns one "filename@applied_at" line per row, ordered by filename.
func ledgerRows(t *testing.T, path, table string) []string {
	t.Helper()
	rows, err := openFresh(t, path).Query("SELECT filename, applied_at FROM " + table + " ORDER BY filename")
	if err != nil {
		t.Fatalf("query %s: %v", table, err)
	}
	defer rows.Close() // the cursor is fully drained below
	var out []string
	for rows.Next() {
		var name string
		var applied int64
		if err := rows.Scan(&name, &applied); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		out = append(out, fmt.Sprintf("%s@%d", name, applied))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	return out
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	t.Parallel()
	if _, err := Open(context.Background(), Config{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Open with empty path error = %v, want ErrInvalidConfig", err)
	}
}

func TestOpenCreatesParentDirectory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nested", "data", "keel.db")
	openTestDB(t, Config{Path: path})
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database file was not created: %v", err)
	}
}

func TestPoolBounds(t *testing.T) {
	t.Parallel()
	db := openTestDB(t, Config{Path: filepath.Join(t.TempDir(), "keel.db"), MaxReaders: 2})
	if got := db.Writer().Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("writer MaxOpenConnections = %d, want 1", got)
	}
	if got := db.Reader().Stats().MaxOpenConnections; got != 2 {
		t.Fatalf("reader MaxOpenConnections = %d, want 2", got)
	}
}

func TestReaderCannotWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestDB(t, Config{Path: filepath.Join(t.TempDir(), "keel.db")})
	mustExec(t, db.Writer(), "CREATE TABLE t (n INTEGER)")
	if _, err := db.Reader().ExecContext(ctx, "INSERT INTO t (n) VALUES (1)"); err == nil {
		t.Fatal("reader accepted a write, want an error")
	}
}

func TestCloseReleasesBothPools(t *testing.T) {
	t.Parallel()
	db := openTestDB(t, Config{Path: filepath.Join(t.TempDir(), "keel.db")})
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := db.Writer().Ping(); err == nil {
		t.Fatal("writer is still usable after Close")
	}
	if err := db.Reader().Ping(); err == nil {
		t.Fatal("reader is still usable after Close")
	}
}

func TestConcurrentWritesDoNotBusy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestDB(t, Config{Path: filepath.Join(t.TempDir(), "keel.db")})
	mustExec(t, db.Writer(), "CREATE TABLE counter (id INTEGER PRIMARY KEY, n INTEGER NOT NULL)")

	const writers = 50
	const perWriter = 20
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range perWriter {
				if _, err := db.Writer().ExecContext(ctx, "INSERT INTO counter (n) VALUES (?)", i); err != nil {
					errs[i] = fmt.Errorf("writer %d insert %d: %w", i, j, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	var count int
	if err := db.Reader().QueryRowContext(ctx, "SELECT count(*) FROM counter").Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if want := writers * perWriter; count != want {
		t.Fatalf("row count = %d, want %d", count, want)
	}
}

const keelLedger = "keel_schema_migrations"

func TestMigrateAppliesOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "keel.db")
	db := openTestDB(t, Config{Path: path})
	migrations := fstest.MapFS{
		"0001_init.sql":     {Data: []byte("CREATE TABLE widget (id INTEGER PRIMARY KEY, name TEXT NOT NULL);")},
		"0002_add_flag.sql": {Data: []byte("ALTER TABLE widget ADD COLUMN flag INTEGER NOT NULL DEFAULT 0;")},
		"notes.md":          {Data: []byte("not a migration")},
	}
	if err := Migrate(ctx, db, "keel", migrations); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	first := ledgerRows(t, path, keelLedger)
	if len(first) != 2 {
		t.Fatalf("ledger after first run = %v, want two rows", first)
	}
	if err := Migrate(ctx, db, "keel", migrations); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	second := ledgerRows(t, path, keelLedger)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("second run changed the ledger: first %v, second %v", first, second)
	}
	// The migrated table answers a query, so the run reached the fresh file.
	fresh := openFresh(t, path)
	var flagCount int
	if err := fresh.QueryRow("SELECT count(*) FROM pragma_table_info('widget') WHERE name = 'flag'").Scan(&flagCount); err != nil {
		t.Fatalf("inspect widget: %v", err)
	}
	if flagCount != 1 {
		t.Fatalf("widget.flag columns = %d, want 1", flagCount)
	}
}

func TestMigrateKeepsNamespacesSeparate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "keel.db")
	db := openTestDB(t, Config{Path: path})
	keel := fstest.MapFS{"0001_k.sql": {Data: []byte("CREATE TABLE keel_only (x INTEGER);")}}
	app := fstest.MapFS{"0001_a.sql": {Data: []byte("CREATE TABLE app_only (x INTEGER);")}}
	if err := Migrate(ctx, db, "keel", keel); err != nil {
		t.Fatalf("Migrate keel: %v", err)
	}
	if err := Migrate(ctx, db, "app", app); err != nil {
		t.Fatalf("Migrate app: %v", err)
	}
	if got := ledgerRows(t, path, keelLedger); len(got) != 1 {
		t.Fatalf("keel ledger = %v, want one row", got)
	}
	if got := ledgerRows(t, path, "app_schema_migrations"); len(got) != 1 {
		t.Fatalf("app ledger = %v, want one row", got)
	}
}

func TestMigrateRejectsBadNamespace(t *testing.T) {
	t.Parallel()
	db := openTestDB(t, Config{Path: filepath.Join(t.TempDir(), "keel.db")})
	for _, namespace := range []string{"", "1bad", "has space", "drop;table", `q"`, "dot.ted"} {
		if err := Migrate(context.Background(), db, namespace, fstest.MapFS{}); !errors.Is(err, ErrInvalidNamespace) {
			t.Fatalf("Migrate namespace %q error = %v, want ErrInvalidNamespace", namespace, err)
		}
	}
}

func TestBackupIsConsistent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	db := openTestDB(t, Config{Path: filepath.Join(dir, "keel.db")})
	mustExec(t, db.Writer(), "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT NOT NULL)")
	mustExec(t, db.Writer(), "INSERT INTO t (v) VALUES ('a'), ('b')")

	dest := filepath.Join(dir, "copies", "copy.db")
	if err := Backup(ctx, db, dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	fresh := openFresh(t, dest)
	var check string
	if err := fresh.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if check != "ok" {
		t.Fatalf("integrity_check = %q, want ok", check)
	}
	var rows int
	if err := fresh.QueryRow("SELECT count(*) FROM t").Scan(&rows); err != nil {
		t.Fatalf("count copied rows: %v", err)
	}
	if rows != 2 {
		t.Fatalf("copied rows = %d, want 2", rows)
	}
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		t.Fatalf("read copies dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("staging file left behind: %s", e.Name())
		}
	}
}

func TestBackupRejectsEmptyDestination(t *testing.T) {
	t.Parallel()
	db := openTestDB(t, Config{Path: filepath.Join(t.TempDir(), "keel.db")})
	if err := Backup(context.Background(), db, ""); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Backup with empty destination error = %v, want ErrInvalidConfig", err)
	}
}

func TestMigrateRejectsNilFS(t *testing.T) {
	t.Parallel()
	db := openTestDB(t, Config{Path: filepath.Join(t.TempDir(), "keel.db")})
	if err := Migrate(context.Background(), db, "keel", nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Migrate with nil fs error = %v, want ErrInvalidConfig", err)
	}
}

func TestMigrateClassifiesApplyError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestDB(t, Config{Path: filepath.Join(t.TempDir(), "keel.db")})
	bad := fstest.MapFS{"0001_bad.sql": {Data: []byte("THIS IS NOT SQL;")}}
	err := Migrate(ctx, db, "keel", bad)
	if !errors.Is(err, ErrMigration) {
		t.Fatalf("apply failure error = %v, want ErrMigration", err)
	}
	var driverErr *moderncsqlite.Error
	if !errors.As(err, &driverErr) {
		t.Fatalf("apply failure error = %v, want the driver error reachable", err)
	}
}

func TestBackupClassifiesDestinationError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	db := openTestDB(t, Config{Path: filepath.Join(dir, "keel.db")})
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	err := Backup(ctx, db, filepath.Join(blocker, "sub", "copy.db"))
	if !errors.Is(err, ErrBackup) {
		t.Fatalf("unwritable destination error = %v, want ErrBackup", err)
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("unwritable destination error = %v, want the os error reachable", err)
	}
}
