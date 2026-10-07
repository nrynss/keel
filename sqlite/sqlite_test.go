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
	"time"
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

// TestOpenSetsDurabilityPragmas pins the pragma pair every store's durability
// promise rests on. The values are read back over the opened pools, so a
// driver that dropped one from the DSN fails here rather than at a consumer.
func TestOpenSetsDurabilityPragmas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestDB(t, Config{Path: filepath.Join(t.TempDir(), "keel.db")})
	pools := []struct {
		name string
		pool *sql.DB
	}{
		{"writer", db.Writer()},
		{"reader", db.Reader()},
	}
	for _, p := range pools {
		var mode string
		if err := p.pool.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatalf("journal_mode on %s: %v", p.name, err)
		}
		if mode != "wal" {
			t.Fatalf("journal_mode on %s = %q, want wal", p.name, mode)
		}
		var sync int
		if err := p.pool.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync); err != nil {
			t.Fatalf("synchronous on %s: %v", p.name, err)
		}
		if sync != 2 {
			t.Fatalf("synchronous on %s = %d, want 2 (FULL)", p.name, sync)
		}
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

// raceTwoOpens releases two Opens of one path through one barrier and returns
// the two handles and errors in goroutine order.
func raceTwoOpens(ctx context.Context, path string, log *slog.Logger) ([2]*DB, [2]error) {
	var (
		dbs   [2]*DB
		errs  [2]error
		wg    sync.WaitGroup
		start = make(chan struct{})
	)
	wg.Add(2)
	for j := range 2 {
		go func(j int) {
			defer wg.Done()
			<-start
			dbs[j], errs[j] = Open(ctx, Config{Path: path, Logger: log})
		}(j)
	}
	close(start)
	wg.Wait()
	return dbs, errs
}

// TestConcurrentOpensOnVirginFileConverge pins the busy window at the pool
// ping. Two concurrent Opens of one virgin file race the journal-mode change,
// and SQLite returns busy for it at once instead of waiting out the busy
// timeout. Both opens must end usable, and no raw busy error may escape. The
// recorded log pins the retry as live code, because a pass with no retried
// ping would prove nothing.
func TestConcurrentOpensOnVirginFileConverge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler := &recordingHandler{}
	log := slog.New(handler)

	const runs = 100
	for i := range runs {
		dbs, errs := raceTwoOpens(ctx, filepath.Join(t.TempDir(), "keel.db"), log)
		for j, err := range errs {
			if err != nil {
				t.Fatalf("run %d open %d: Open: %v", i, j, err)
			}
		}
		for j := range dbs {
			if err := dbs[j].Writer().PingContext(ctx); err != nil {
				t.Fatalf("run %d open %d: writer pool unusable: %v", i, j, err)
			}
			if err := dbs[j].Reader().PingContext(ctx); err != nil {
				t.Fatalf("run %d open %d: reader pool unusable: %v", i, j, err)
			}
		}
		for j := range dbs {
			if err := dbs[j].Close(); err != nil {
				t.Fatalf("run %d open %d: Close: %v", i, j, err)
			}
		}
	}
	if handler.count(busyPingRecord) == 0 {
		t.Fatal("no run recorded a retried ping, so the racing pair never met the busy window")
	}
}

// TestOpenCancelledContextFailsFast pins the fast side of the retry. A dead
// context must surface as the ping error with context.Canceled reachable,
// without exhausting the retry window and without a retry record.
func TestOpenCancelledContextFailsFast(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handler := &recordingHandler{}
	started := time.Now()
	_, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "keel.db"), Logger: slog.New(handler)})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("Open on a cancelled context returned nil, want an error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Open error = %v, want context.Canceled reachable", err)
	}
	if handler.count(busyPingRecord) != 0 {
		t.Fatal("a cancelled context recorded a ping retry")
	}
	if elapsed >= pingRetryDelay*pingRetryAttempts {
		t.Fatalf("cancelled Open took %v, the retry window did not break early", elapsed)
	}
}

// TestOpenNonBusyPingErrorSurfaces pins the narrow class of the retry. A ping
// that fails for any other reason must surface once, with the driver error
// still reachable behind the ping wrap, and must never retry. The elapsed
// bound is the immediacy pin: the honest path answers in microseconds, and a
// fifth of the retry window leaves room for scheduler noise while any real
// retry of this failure would overrun it.
func TestOpenNonBusyPingErrorSurfaces(t *testing.T) {
	t.Parallel()
	handler := &recordingHandler{}
	blocked := filepath.Join(t.TempDir(), "db")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatalf("create blocking directory: %v", err)
	}
	started := time.Now()
	_, err := Open(context.Background(), Config{Path: blocked, Logger: slog.New(handler)})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("Open over a directory returned nil, want an error")
	}
	if !strings.HasPrefix(err.Error(), "sqlite: open: ping: ") {
		t.Fatalf("directory Open error = %v, want the ping wrap preserved", err)
	}
	var driverErr *moderncsqlite.Error
	if !errors.As(err, &driverErr) {
		t.Fatalf("directory Open error = %v, want the driver error reachable", err)
	}
	if busyPingError(err) {
		t.Fatalf("directory Open error = %v, the retry must not cover this class", err)
	}
	if handler.count(busyPingRecord) != 0 {
		t.Fatal("a non-busy ping failure recorded a retry")
	}
	if elapsed >= pingRetryDelay*pingRetryAttempts/5 {
		t.Fatalf("directory Open took %v, a non-busy failure retried instead of surfacing", elapsed)
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

// concurrentApplyRecord is the log message the runner writes when it skips a
// file another open has just applied.
const concurrentApplyRecord = "sqlite: migration applied by a concurrent open"

// recordingHandler collects log record messages, so a test can tell whether
// the runner took its concurrent-open path.
type recordingHandler struct {
	mu       sync.Mutex
	messages []string
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, r.Message)
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

func (h *recordingHandler) count(message string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, m := range h.messages {
		if m == message {
			n++
		}
	}
	return n
}

// raceTwoMigrates runs Migrate twice over one namespace from two goroutines
// released at one barrier, and returns the two errors in goroutine order.
func raceTwoMigrates(ctx context.Context, a, b *DB, namespace string, migrations fstest.MapFS) [2]error {
	var (
		errs  [2]error
		wg    sync.WaitGroup
		start = make(chan struct{})
	)
	dbs := [2]*DB{a, b}
	wg.Add(2)
	for j := range 2 {
		go func(j int) {
			defer wg.Done()
			<-start
			errs[j] = Migrate(ctx, dbs[j], namespace, migrations)
		}(j)
	}
	close(start)
	wg.Wait()
	return errs
}

// racingMigrations builds the two files a racing pair applies.
func racingMigrations() fstest.MapFS {
	return fstest.MapFS{
		"0001_widgets.sql": {Data: []byte("CREATE TABLE widgets (id INTEGER PRIMARY KEY, name TEXT NOT NULL)")},
		"0002_gadgets.sql": {Data: []byte("CREATE TABLE gadgets (id INTEGER PRIMARY KEY, widget_id INTEGER NOT NULL)")},
	}
}

// TestMigrateRacingFirstOpensConverge pins the first-open race inside one
// handle. Two concurrent first migrations of one virgin file must both
// succeed, leave exactly one schema behind, and never leak a driver error.
// The runs also pin the retry path as live code, because a pass with no
// recorded collision would prove nothing.
func TestMigrateRacingFirstOpensConverge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	migrations := racingMigrations()
	handler := &recordingHandler{}

	const runs = 50
	for i := range runs {
		dir := t.TempDir()
		path := filepath.Join(dir, "keel.db")
		db, err := Open(ctx, Config{Path: path, Logger: slog.New(handler)})
		if err != nil {
			t.Fatalf("run %d: Open: %v", i, err)
		}
		errs := raceTwoMigrates(ctx, db, db, "widgets", migrations)
		for j, err := range errs {
			if err != nil {
				t.Fatalf("run %d open %d: Migrate: %v", i, j, err)
			}
		}
		rows := ledgerRows(t, path, "widgets_schema_migrations")
		if len(rows) != 2 {
			t.Fatalf("run %d: ledger = %v, want one row per file", i, rows)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("run %d: Close: %v", i, err)
		}
	}
	if handler.count(concurrentApplyRecord) == 0 {
		t.Fatal("no run recorded a concurrent apply, so the racing pair never collided")
	}
}

// TestMigrateRacingFirstOpensAcrossHandlesConverge pins the first-open race
// across two handles, the shape a restart overlapping a health check meets.
// Both migrations must succeed and the file must hold exactly one schema.
func TestMigrateRacingFirstOpensAcrossHandlesConverge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	migrations := racingMigrations()

	const runs = 25
	for i := range runs {
		path := filepath.Join(t.TempDir(), "keel.db")
		first := openTestDB(t, Config{Path: path})
		second := openTestDB(t, Config{Path: path})
		errs := raceTwoMigrates(ctx, first, second, "widgets", migrations)
		for j, err := range errs {
			if err != nil {
				t.Fatalf("run %d open %d: Migrate: %v", i, j, err)
			}
		}
		rows := ledgerRows(t, path, "widgets_schema_migrations")
		if len(rows) != 2 {
			t.Fatalf("run %d: ledger = %v, want one row per file", i, rows)
		}
		fresh := openFresh(t, path)
		var tables int
		if err := fresh.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('widgets', 'gadgets')").Scan(&tables); err != nil {
			t.Fatalf("run %d: inspect schema: %v", i, err)
		}
		if tables != 2 {
			t.Fatalf("run %d: schema tables = %d, want 2", i, tables)
		}
	}
}

// TestMigrateRacingIncompatibleNamespacesFailLoudly pins the honest refusal.
// Two namespaces that migrate genuinely incompatible schemas into one file
// must end with exactly one classified failure, and the loser's own ledger
// must stay empty, because no applied file vouches for the collision.
func TestMigrateRacingIncompatibleNamespacesFailLoudly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "keel.db")
	first := openTestDB(t, Config{Path: path})
	second := openTestDB(t, Config{Path: path})
	namespaces := [2]string{"left", "right"}
	schemas := [2]fstest.MapFS{
		{"0001_shared.sql": {Data: []byte("CREATE TABLE shared (a INTEGER PRIMARY KEY)")}},
		{"0001_shared.sql": {Data: []byte("CREATE TABLE shared (b TEXT PRIMARY KEY)")}},
	}

	handles := [2]*DB{first, second}
	var (
		errs  [2]error
		wg    sync.WaitGroup
		start = make(chan struct{})
	)
	wg.Add(2)
	for j := range 2 {
		go func(j int) {
			defer wg.Done()
			<-start
			errs[j] = Migrate(ctx, handles[j], namespaces[j], schemas[j])
		}(j)
	}
	close(start)
	wg.Wait()

	failures := 0
	for j, err := range errs {
		ledger := namespaces[j] + "_schema_migrations"
		if err == nil {
			if rows := ledgerRows(t, path, ledger); len(rows) != 1 {
				t.Fatalf("winner %s: ledger = %v, want one row", namespaces[j], rows)
			}
			continue
		}
		failures++
		if !errors.Is(err, ErrMigration) {
			t.Fatalf("loser %s: Migrate error = %v, want ErrMigration", namespaces[j], err)
		}
		if rows := ledgerRows(t, path, ledger); len(rows) != 0 {
			t.Fatalf("loser %s: ledger = %v, want no recorded file", namespaces[j], rows)
		}
	}
	if failures != 1 {
		t.Fatalf("incompatible schemas produced %d failures, want exactly one", failures)
	}
}

// TestMigrateConflictWithoutLedgerRowStillFails pins the loud side of the
// retry rule with no race involved. A migration that collides with a table
// nobody recorded must fail, because no ledger row vouches for the file.
func TestMigrateConflictWithoutLedgerRowStillFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestDB(t, Config{Path: filepath.Join(t.TempDir(), "keel.db")})
	mustExec(t, db.Writer(), "CREATE TABLE widget (id INTEGER PRIMARY KEY)")
	migrations := fstest.MapFS{
		"0001_widget.sql": {Data: []byte("CREATE TABLE widget (id INTEGER PRIMARY KEY)")},
	}
	err := Migrate(ctx, db, "keel", migrations)
	if !errors.Is(err, ErrMigration) {
		t.Fatalf("unrecorded conflict error = %v, want ErrMigration", err)
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
