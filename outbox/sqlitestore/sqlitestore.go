// Package sqlitestore implements the outbox store over SQLite.
//
// The package owns one schema, applied through the sqlite package's
// migration runner under its own namespace, so the ledger records what ran
// and a second open changes nothing. Reads go through the read pool and
// every write through the single write connection, so concurrent adds and
// passes queue rather than race for the file lock.
//
// An entry is one row, and its insertion sequence is the row's own rowid.
// Add inserts the row and commits before it returns, so a process that dies
// right after Add replays the entry on the next open rather than losing it.
// The rowid also fixes the replay order, so Pending returns the same order
// even when every entry is stamped with one clock reading.
//
// Delivered deletes the rows it is given, and deletion is this store's
// record of delivery. The table therefore holds only entries still owed,
// which keeps the recurring read of a pass small and leaves no delivered
// tail to prune. A delivery the sink never accepts stays in the table and
// shows up in the exhausted read, so a stuck sink is visible for as long as
// it takes.
//
// This package is one of the few allowed to import the SQLite driver. The
// batching, the pacing and the attempt caps stay in the outbox package.
package sqlitestore

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/nrynss/keel/outbox"
	"github.com/nrynss/keel/sqlite"
)

// schemaNamespace is the migration ledger namespace this package owns. It
// shares the database file with every other namespace without colliding,
// because each namespace keeps its own ledger.
const schemaNamespace = "outbox"

//go:embed migrations/*.sql
var migrations embed.FS

// ErrInvalid is returned by Open for unusable configuration, such as a nil
// database.
var ErrInvalid = errors.New("sqlitestore: invalid config")

// Config configures Open.
type Config struct {
	// DB is the open database the store writes through. It must not be
	// nil, and this package never closes it.
	DB *sqlite.DB

	// Namespace overrides the migration ledger namespace. Empty means the
	// one this package owns, which lets two stores share one file.
	Namespace string
}

// Store keeps outbox entries over one database. Create it with Open,
// because the zero value has no database. Store is safe for concurrent use.
type Store struct {
	db *sqlite.DB
}

// A Store satisfies outbox.Store, so an Outbox can take it as the thing its
// entries are remembered in.
var _ outbox.Store = (*Store)(nil)

// Open applies the outbox schema to cfg.DB and returns the store. The
// schema arrives as ordered SQL files, so the migration runner owns what
// ran and records each file once.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("sqlitestore: open: %w: nil db", ErrInvalid)
	}
	namespace := cfg.Namespace
	if namespace == "" {
		namespace = schemaNamespace
	}
	schema, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: open: %w", err)
	}
	if err := sqlite.Migrate(ctx, cfg.DB, namespace, schema); err != nil {
		return nil, fmt.Errorf("sqlitestore: open: %w", err)
	}
	return &Store{db: cfg.DB}, nil
}

// Add appends e as one row and commits the insert before it returns, so a
// process that dies after Add replays the entry. The sequence is left to
// the table, which hands out the next rowid, so the row lands after
// everything already stored. An absent payload is stored as a zero-length
// blob, because a column that can be read back empty is not a missing one.
func (s *Store) Add(ctx context.Context, e outbox.Entry) error {
	payload := e.Payload
	if payload == nil {
		payload = []byte{}
	}
	_, err := s.db.Writer().ExecContext(ctx,
		`INSERT INTO outbox_entry (id, payload, failures, added_at) VALUES (?, ?, ?, ?)`,
		e.ID, payload, e.Failures, e.AddedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("sqlitestore: add %s: %w", e.ID, err)
	}
	return nil
}

// Pending returns at most limit entries still owed with fewer than
// maxFailures recorded failures, oldest first. The limit rides in the SQL,
// so a read of one row costs one row. A negative limit removes the bound,
// which is how an unbounded drain asks for everything, and zero returns
// nothing.
func (s *Store) Pending(ctx context.Context, maxFailures, limit int) ([]outbox.Entry, error) {
	return s.query(ctx,
		`SELECT id, payload, failures, added_at FROM outbox_entry WHERE failures < ? ORDER BY seq LIMIT ?`,
		maxFailures, limit, "pending")
}

// Exhausted returns every entry still owed with at least maxFailures
// recorded failures, oldest first. The read is the reporting read a caller
// runs to see what is stuck, so it takes no limit.
func (s *Store) Exhausted(ctx context.Context, maxFailures int) ([]outbox.Entry, error) {
	return s.query(ctx,
		`SELECT id, payload, failures, added_at FROM outbox_entry WHERE failures >= ? ORDER BY seq LIMIT ?`,
		maxFailures, -1, "exhausted")
}

// Delivered removes the named entries, which is this store's record of
// their delivery. The removals share one transaction, so a batch is retired
// whole or replayed whole. A name with no row is not an error.
func (s *Store) Delivered(ctx context.Context, ids []string) error {
	return s.eachID(ctx, ids, `DELETE FROM outbox_entry WHERE id = ?`, "deliver")
}

// RecordFailures adds one recorded failure to each named entry. The updates
// share one transaction, so a refused batch ages as one. A name with no row
// is not an error.
func (s *Store) RecordFailures(ctx context.Context, ids []string) error {
	return s.eachID(ctx, ids, `UPDATE outbox_entry SET failures = failures + 1 WHERE id = ?`, "record failures")
}

// eachID runs one statement per id inside a single transaction, so a batch
// lands whole. A statement that touches no row is not an error.
func (s *Store) eachID(ctx context.Context, ids []string, stmt, what string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlitestore: %s: %w", what, err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	for _, entryID := range ids {
		if _, err := tx.ExecContext(ctx, stmt, entryID); err != nil {
			return fmt.Errorf("sqlitestore: %s %s: %w", what, entryID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlitestore: %s: %w", what, err)
	}
	return nil
}

// query runs one listing read and decodes every row under the name a caller
// would use for it in an error.
func (s *Store) query(ctx context.Context, query string, maxFailures, limit int, what string) ([]outbox.Entry, error) {
	rows, err := s.db.Reader().QueryContext(ctx, query, maxFailures, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: %s: %w", what, err)
	}
	defer rows.Close() // the cursor is drained before the caller returns
	var out []outbox.Entry
	for rows.Next() {
		e, err := scanEntry(rows, what)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlitestore: %s: %w", what, err)
	}
	return out, nil
}

// rowScanner is the Scan subset shared by *sql.Row and *sql.Rows, so one
// decode serves the single-row and the listing read.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanEntry decodes one row. This package wrote every column, so a decode
// failure is corruption and keeps its own cause.
func scanEntry(row rowScanner, what string) (outbox.Entry, error) {
	var (
		e        outbox.Entry
		failures int
		addedAt  int64
	)
	if err := row.Scan(&e.ID, &e.Payload, &failures, &addedAt); err != nil {
		return outbox.Entry{}, fmt.Errorf("sqlitestore: %s: %w", what, err)
	}
	e.Failures = failures
	e.AddedAt = time.Unix(0, addedAt)
	return e, nil
}
