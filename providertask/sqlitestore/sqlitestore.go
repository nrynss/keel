// Package sqlitestore implements the providertask store over SQLite.
//
// The package owns one schema, applied through the sqlite package's
// migration runner under its own namespace, so the ledger records what ran
// and a second open changes nothing. Reads go through the read pool and
// every write through the single write connection, so concurrent claims
// queue rather than race for the file lock.
//
// A claim is one row keyed by the caller's idempotency key. Claim inserts
// the row inside one transaction that also reads it back, and the unique
// key decides the winner, so concurrent claims pick exactly one creator
// even across processes. The insert and the read share one transaction, so
// a loser never observes a moment where its own insert erased the winner's
// row. The row records the provider task id, the terminal verdict and the
// creation time, which is the whole state a resume needs.
//
// Release deletes a row while it still carries no task id, and Record
// writes a task id only while the row carries none. Together they keep one
// invariant: a key that names a task keeps naming it, so a repeated Run
// can never point an old key at a new task.
//
// This package is one of the few allowed to import the SQLite driver. The
// polling, the pacing and the meter stay in the providertask package.
package sqlitestore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/nrynss/keel/providertask"
	"github.com/nrynss/keel/sqlite"
)

// schemaNamespace is the migration ledger namespace this package owns. It
// shares the database file with every other namespace without colliding,
// because each namespace keeps its own ledger.
const schemaNamespace = "providertask"

//go:embed migrations/*.sql
var migrations embed.FS

// ErrInvalid is returned by Open and by Finish for unusable input, such as
// a nil database or a terminal record that names no terminal state.
var ErrInvalid = errors.New("sqlitestore: invalid config")

// rowState and rowTaskID are the column values a fresh claim starts with.
const (
	rowState  = string(providertask.StateRunning)
	rowTaskID = ""
)

// Config configures Open.
type Config struct {
	// DB is the open database the store writes through. It must not be
	// nil, and this package never closes it.
	DB *sqlite.DB

	// Namespace overrides the migration ledger namespace. Empty means the
	// one this package owns, which lets two stores share one file.
	Namespace string
}

// Store keeps providertask claims over one database. Create it with Open,
// because the zero value has no database. Store is safe for concurrent
// use.
type Store struct {
	db *sqlite.DB
}

// storeMatches pins Store to the shape providertask drives, so a signature
// drift breaks the build here rather than at a call site.
var _ providertask.Store = (*Store)(nil)

// Open applies the providertask schema to cfg.DB and returns the store.
// The schema arrives as ordered SQL files, so the migration runner owns
// what ran and records each file once.
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

// Claim inserts a row for key when none exists and reports the row either
// way. The insert and the read share one transaction, and the primary key
// decides the winner, so exactly one concurrent caller reports created
// true. A row this call did not insert is returned unchanged, because a
// claim belongs to the Run that made it.
func (s *Store) Claim(ctx context.Context, key string, now time.Time) (providertask.Claim, bool, error) {
	tx, err := s.db.Writer().BeginTx(ctx, nil)
	if err != nil {
		return providertask.Claim{}, false, fmt.Errorf("sqlitestore: claim %s: %w", key, err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	result, err := tx.ExecContext(ctx,
		`INSERT INTO providertask_task (key, task_id, state, error_code, created_at)
			VALUES (?, ?, ?, '', ?)
			ON CONFLICT (key) DO NOTHING`,
		key, rowTaskID, rowState, now.UnixNano())
	if err != nil {
		return providertask.Claim{}, false, fmt.Errorf("sqlitestore: claim %s: %w", key, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return providertask.Claim{}, false, fmt.Errorf("sqlitestore: claim %s: %w", key, err)
	}
	claim, err := scanClaim(tx.QueryRowContext(ctx,
		`SELECT key, task_id, state, error_code, created_at FROM providertask_task WHERE key = ?`, key))
	if err != nil {
		return providertask.Claim{}, false, fmt.Errorf("sqlitestore: claim %s: %w", key, err)
	}
	if err := tx.Commit(); err != nil {
		return providertask.Claim{}, false, fmt.Errorf("sqlitestore: claim %s: %w", key, err)
	}
	return claim, inserted == 1, nil
}

// Record attaches taskID to the key's row, and writes only while the row
// still carries no task id. A key therefore never moves to a different
// task. A repeat with the recorded id changes nothing and is not an
// error. An unknown key reports an error matching
// providertask.ErrUnknownKey.
func (s *Store) Record(ctx context.Context, key, taskID string) error {
	result, err := s.db.Writer().ExecContext(ctx,
		`UPDATE providertask_task SET task_id = ?, state = ?, error_code = ''
			WHERE key = ? AND task_id = ''`,
		taskID, rowState, key)
	if err != nil {
		return fmt.Errorf("sqlitestore: record %s: %w", key, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlitestore: record %s: %w", key, err)
	}
	if updated == 1 {
		return nil
	}
	// The update matched no row, which is an unknown key or a key that
	// already carries a task id. The read tells the two apart. A repeat of
	// the recorded id is the idempotent case and is not an error.
	claim, err := s.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("sqlitestore: record %s: %w", key, err)
	}
	if claim.TaskID == taskID {
		return nil
	}
	return fmt.Errorf("sqlitestore: record %s: key already records task %s", key, claim.TaskID)
}

// Get returns the row stored under key, or an error matching
// providertask.ErrUnknownKey.
func (s *Store) Get(ctx context.Context, key string) (providertask.Claim, error) {
	claim, err := scanClaim(s.db.Reader().QueryRowContext(ctx,
		`SELECT key, task_id, state, error_code, created_at FROM providertask_task WHERE key = ?`, key))
	if err != nil {
		return providertask.Claim{}, unknownKey(key, err)
	}
	return claim, nil
}

// Finish records the provider's terminal verdict on the key's row. A state
// that names no terminal verdict reports ErrInvalid, and an unknown key
// reports an error matching providertask.ErrUnknownKey.
func (s *Store) Finish(ctx context.Context, key string, state providertask.State, code string) error {
	if state != providertask.StateSucceeded && state != providertask.StateFailed {
		return fmt.Errorf("sqlitestore: finish %s: %w: state %q is not terminal", key, ErrInvalid, state)
	}
	result, err := s.db.Writer().ExecContext(ctx,
		`UPDATE providertask_task SET state = ?, error_code = ? WHERE key = ?`,
		string(state), code, key)
	if err != nil {
		return fmt.Errorf("sqlitestore: finish %s: %w", key, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlitestore: finish %s: %w", key, err)
	}
	if updated == 0 {
		return fmt.Errorf("sqlitestore: finish %s: %w", key, providertask.ErrUnknownKey)
	}
	return nil
}

// Release removes the key's row while it still carries no task id, and
// changes nothing once a task id is recorded. It reports whether it
// removed a row. The guard lives in the statement, so a release can never
// erase a task a key already names.
func (s *Store) Release(ctx context.Context, key string) (bool, error) {
	result, err := s.db.Writer().ExecContext(ctx,
		`DELETE FROM providertask_task WHERE key = ? AND task_id = ''`, key)
	if err != nil {
		return false, fmt.Errorf("sqlitestore: release %s: %w", key, err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("sqlitestore: release %s: %w", key, err)
	}
	return removed == 1, nil
}

// unknownKey classifies one read failure. A missing row is the sentinel
// the providertask package matches on, and every other fault keeps its own
// cause, so a closed database is never mistaken for a key that is absent.
func unknownKey(key string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("sqlitestore: get %s: %w", key, providertask.ErrUnknownKey)
	}
	return fmt.Errorf("sqlitestore: get %s: %w", key, err)
}

// scanClaim decodes one claim row. This package wrote every column, so a
// decode failure is corruption and keeps its own cause.
func scanClaim(row *sql.Row) (providertask.Claim, error) {
	var (
		claim     providertask.Claim
		taskID    string
		state     string
		errorCode string
		createdAt int64
	)
	if err := row.Scan(&claim.Key, &taskID, &state, &errorCode, &createdAt); err != nil {
		return providertask.Claim{}, err
	}
	claim.TaskID = taskID
	claim.State = providertask.State(state)
	claim.Code = errorCode
	claim.CreatedAt = time.Unix(0, createdAt)
	return claim, nil
}
