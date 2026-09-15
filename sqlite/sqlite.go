// Package sqlite opens the SQLite databases that every Keel store and every
// consumer share.
//
// One database file serves both Keel and the application that embeds it, so
// every handle carries the same pragmas. Foreign keys are on, so a delete
// cascades to its children. The busy timeout rides out a lock held by another
// process. WAL mode keeps a crash mid-write from corrupting the last
// committed transaction, and synchronous NORMAL is safe under WAL.
//
// A handle holds two pools over the one file. The writer pool holds a single
// connection, because SQLite admits one writer at a time and a second
// connection could only ever return SQLITE_BUSY. The reader pool holds
// several connections, each opened with query_only so a read can never write.
//
// Migrations are namespaced. Each namespace keeps its own version table, so
// Keel stores and the embedding application share one database file without
// colliding.
//
// This package is one of the few allowed to import the SQLite driver.
// Everything else reaches SQLite through it.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	// Register the pure-Go driver under the name this package opens, so a
	// build with cgo disabled still carries SQLite.
	_ "modernc.org/sqlite"
)

// driverName is the name the SQLite driver registers itself under.
const driverName = "sqlite"

// defaultBusyTimeout is how long a statement waits for a lock before it
// fails with SQLITE_BUSY. Five seconds covers a checkpoint or a slow write in
// another process.
const defaultBusyTimeout = 5 * time.Second

// defaultMaxReaders caps the read pool when Config does not set one. Four
// readers overlap enough work without holding many snapshots open, since each
// open snapshot delays a WAL checkpoint.
const defaultMaxReaders = 4

// versionTableSuffix names the per-namespace ledger table. The table records
// the migrations a namespace has applied.
const versionTableSuffix = "_schema_migrations"

var (
	// ErrInvalidConfig is returned by Open, Migrate and Backup for an argument
	// they cannot honour.
	ErrInvalidConfig = errors.New("sqlite: invalid config")

	// ErrInvalidNamespace is returned by Migrate when a namespace cannot
	// name a SQL identifier.
	ErrInvalidNamespace = errors.New("sqlite: invalid namespace")

	// ErrMigration is returned by Migrate when a migration file cannot be
	// read, staged, or applied.
	ErrMigration = errors.New("sqlite: migration")

	// ErrBackup is returned by Backup when the copy cannot be written.
	ErrBackup = errors.New("sqlite: backup")
)

// Config configures Open. The zero value is usable except that Path must be
// set: an empty Path is rejected rather than opening a file named "".
type Config struct {
	// Path is the database file. Open creates the file when absent and
	// creates its parent directory when missing.
	Path string

	// BusyTimeout is how long a statement waits for a lock before it fails
	// with SQLITE_BUSY. Zero means five seconds.
	BusyTimeout time.Duration

	// MaxReaders caps the read pool. Zero means four connections. The
	// writer pool always holds one connection.
	MaxReaders int

	// Logger receives one record per applied migration. Nil means
	// slog.Default.
	Logger *slog.Logger

	// Now supplies the applied time a migration records. Nil means
	// time.Now.
	Now func() time.Time
}

// DB is an open database. It holds one write connection and a read pool over
// the same file. Create it with Open and release it with Close. A DB is safe
// for concurrent use.
type DB struct {
	writer *sql.DB
	reader *sql.DB
	log    *slog.Logger
	now    func() time.Time
}

// Open opens, creating if absent, the SQLite database at cfg.Path. It returns
// a handle with one write connection and a read pool, both carrying the
// pragmas this package documents. The caller closes the handle with Close.
func Open(ctx context.Context, cfg Config) (*DB, error) {
	if cfg.Path == "" {
		return nil, fmt.Errorf("sqlite: open: %w: path must not be empty", ErrInvalidConfig)
	}
	abs, err := filepath.Abs(cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open: resolve path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, fmt.Errorf("sqlite: open: create data dir: %w", err)
	}
	timeout := cfg.BusyTimeout
	if timeout <= 0 {
		timeout = defaultBusyTimeout
	}
	maxReaders := cfg.MaxReaders
	if maxReaders <= 0 {
		maxReaders = defaultMaxReaders
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	writer, err := openPool(ctx, abs, timeout, 1, false)
	if err != nil {
		return nil, err
	}
	reader, err := openPool(ctx, abs, timeout, maxReaders, true)
	if err != nil {
		writer.Close() // the ping failed, so the writer swallowed nothing
		return nil, err
	}
	return &DB{writer: writer, reader: reader, log: logger, now: now}, nil
}

// openPool opens one pool over the database at abs. A read pool carries
// query_only on every connection. The pool opens exactly limit connections.
func openPool(ctx context.Context, abs string, timeout time.Duration, limit int, readOnly bool) (*sql.DB, error) {
	pool, err := sql.Open(driverName, dsn(abs, timeout, readOnly))
	if err != nil {
		return nil, fmt.Errorf("sqlite: open: %w", err)
	}
	pool.SetMaxOpenConns(limit)
	if err := pool.PingContext(ctx); err != nil {
		pool.Close() // the ping failed before a session was usable, nothing to drain
		return nil, fmt.Errorf("sqlite: open: ping: %w", err)
	}
	return pool, nil
}

// dsn builds the file URI that Open hands the driver. The pragmas travel in
// the query string so every pooled connection gets them at open, not just the
// first. The path goes in absolute because SQLite's URI parser rejects a
// relative path, and pinning it guards against a later chdir moving the file
// out from under an open handle.
func dsn(abs string, timeout time.Duration, readOnly bool) string {
	ms := timeout.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	q := url.Values{}
	q.Set("_foreign_keys", "on")
	q.Set("_journal_mode", "WAL")
	q.Set("_synchronous", "NORMAL")
	q.Set("_busy_timeout", strconv.FormatInt(ms, 10))
	if readOnly {
		q.Set("_query_only", "on")
	}
	return (&url.URL{Scheme: "file", Path: abs, RawQuery: q.Encode()}).String()
}

// Writer returns the write pool. It holds one connection, so concurrent
// writers queue rather than race for the file lock.
func (db *DB) Writer() *sql.DB { return db.writer }

// Reader returns the read pool. Every connection carries query_only, so a
// statement sent here can never write.
func (db *DB) Reader() *sql.DB { return db.reader }

// Close closes both pools. It returns the joined error of the two closes, or
// nil when both succeed.
func (db *DB) Close() error {
	return errors.Join(db.reader.Close(), db.writer.Close())
}

// Migrate applies every .sql file in fsys, in filename order, to db. Each
// namespace keeps its own version table, so two namespaces share one database
// file without colliding. A file already recorded in the table is skipped, so
// a second run changes nothing.
//
// Each file runs in its own transaction together with its version row, so a
// file that fails partway leaves nothing behind. A file must not contain its
// own BEGIN or COMMIT.
func Migrate(ctx context.Context, db *DB, namespace string, fsys fs.FS) error {
	if !validNamespace(namespace) {
		return fmt.Errorf("sqlite: migrate: %w: %q", ErrInvalidNamespace, namespace)
	}
	if fsys == nil {
		return fmt.Errorf("sqlite: migrate: %w: fsys must not be nil", ErrInvalidConfig)
	}
	names, err := sqlFiles(fsys)
	if err != nil {
		return fmt.Errorf("sqlite: migrate: %w: %w", ErrMigration, err)
	}
	table := namespace + versionTableSuffix
	if err := db.ensureVersionTable(ctx, table); err != nil {
		return err
	}
	applied, err := db.appliedVersions(ctx, table)
	if err != nil {
		return err
	}
	for _, name := range names {
		if applied[name] {
			continue
		}
		if err := db.applyMigration(ctx, table, namespace, fsys, name); err != nil {
			return err
		}
	}
	return nil
}

// ensureVersionTable creates the namespace ledger table when absent.
func (db *DB) ensureVersionTable(ctx context.Context, table string) error {
	stmt := "CREATE TABLE IF NOT EXISTS " + quoteIdent(table) + ` (
	filename   TEXT PRIMARY KEY,
	applied_at INTEGER NOT NULL
)`
	if _, err := db.writer.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("sqlite: migrate: create %s: %w: %w", table, ErrMigration, err)
	}
	return nil
}

// appliedVersions reads the set of filenames the namespace has applied.
func (db *DB) appliedVersions(ctx context.Context, table string) (map[string]bool, error) {
	rows, err := db.writer.QueryContext(ctx, "SELECT filename FROM "+quoteIdent(table))
	if err != nil {
		return nil, fmt.Errorf("sqlite: migrate: read %s: %w: %w", table, ErrMigration, err)
	}
	defer rows.Close() // the row cursor is drained before the caller returns
	applied := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("sqlite: migrate: scan %s: %w: %w", table, ErrMigration, err)
		}
		applied[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: migrate: read %s: %w: %w", table, ErrMigration, err)
	}
	return applied, nil
}

// applyMigration runs one file and records it in the namespace ledger inside a
// single transaction.
func (db *DB) applyMigration(ctx context.Context, table, namespace string, fsys fs.FS, name string) error {
	body, err := fs.ReadFile(fsys, name)
	if err != nil {
		return fmt.Errorf("sqlite: migrate: read %s: %w: %w", name, ErrMigration, err)
	}
	tx, err := db.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: migrate: begin %s: %w: %w", name, ErrMigration, err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	if _, err := tx.ExecContext(ctx, string(body)); err != nil {
		return fmt.Errorf("sqlite: migrate: apply %s: %w: %w", name, ErrMigration, err)
	}
	insert := "INSERT INTO " + quoteIdent(table) + " (filename, applied_at) VALUES (?, ?)"
	if _, err := tx.ExecContext(ctx, insert, name, db.now().UnixMilli()); err != nil {
		return fmt.Errorf("sqlite: migrate: record %s: %w: %w", name, ErrMigration, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: migrate: commit %s: %w: %w", name, ErrMigration, err)
	}
	db.log.InfoContext(ctx, "sqlite: applied migration", "namespace", namespace, "filename", name)
	return nil
}

// Backup writes a consistent copy of the database at db to dest. It stages
// the copy with VACUUM INTO a temporary file beside dest, syncs it, then
// renames it into place, so a reader of dest never observes a half written
// file. Backup uses the write connection, so it serialises with live writes.
func Backup(ctx context.Context, db *DB, dest string) error {
	if dest == "" {
		return fmt.Errorf("sqlite: backup: %w: destination must not be empty", ErrInvalidConfig)
	}
	abs, err := filepath.Abs(dest)
	if err != nil {
		return fmt.Errorf("sqlite: backup: resolve destination: %w: %w", ErrBackup, err)
	}
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("sqlite: backup: create destination dir: %w: %w", ErrBackup, err)
	}
	stage, err := stageName(dir, filepath.Base(abs))
	if err != nil {
		return err
	}
	defer func() {
		_ = os.Remove(stage) // the staged file is gone once the rename succeeds
	}()
	if _, err := db.writer.ExecContext(ctx, "VACUUM INTO ?", stage); err != nil {
		return fmt.Errorf("sqlite: backup: vacuum into %s: %w: %w", stage, ErrBackup, err)
	}
	if err := syncFile(stage); err != nil {
		return err
	}
	if err := os.Rename(stage, abs); err != nil {
		return fmt.Errorf("sqlite: backup: rename into place: %w: %w", ErrBackup, err)
	}
	return nil
}

// stageName reserves an unused temporary path in dir, named after the
// destination so a leftover file names its own backup.
func stageName(dir, base string) (string, error) {
	f, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("sqlite: backup: reserve staging file: %w: %w", ErrBackup, err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("sqlite: backup: reserve staging file: %w: %w", ErrBackup, err)
	}
	// VACUUM INTO refuses a destination that already exists and writes the
	// copy itself, so the reserved name must be released first.
	if err := os.Remove(name); err != nil {
		return "", fmt.Errorf("sqlite: backup: release staging file: %w: %w", ErrBackup, err)
	}
	return name, nil
}

// syncFile flushes path to stable storage, so a crash after Backup returns
// cannot leave the renamed file shorter than the bytes VACUUM INTO wrote.
func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("sqlite: backup: open copy for sync: %w: %w", ErrBackup, err)
	}
	if err := f.Sync(); err != nil {
		f.Close() // nothing further to flush after a failed sync
		return fmt.Errorf("sqlite: backup: sync copy: %w: %w", ErrBackup, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("sqlite: backup: close copy: %w: %w", ErrBackup, err)
	}
	return nil
}

// sqlFiles lists the .sql files at the root of fsys in filename order.
func sqlFiles(fsys fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".sql" {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// validNamespace reports whether s can name a SQL identifier: a letter or
// underscore, then letters, digits or underscores.
func validNamespace(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// quoteIdent wraps a SQL identifier in double quotes and doubles any inner
// quote, so a validated namespace cannot break out of the identifier
// position.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
