// Package sqlitestore implements the providertask store over SQLite.
//
// The package owns one schema, applied through the sqlite package's
// migration runner under its own namespace, so the ledger records what ran
// and a second open changes nothing. Reads go through the read pool and
// every write through the single write connection, so concurrent claims
// queue rather than race for the file lock.
//
// A claim is one row keyed by the caller's idempotency key. The row
// carries the whole cross-process promise: the task id, the terminal
// verdict, the creation time, and the lease of the process that is driving
// the task. Claim inserts the row inside one transaction that also reads
// it back, and the unique key decides the winner, so concurrent claims
// pick exactly one creator. A provider task is therefore created once and
// settled once, across processes, including after a crash or a leader
// stall past its lease. One process drives a recorded task at a time.
// Another takes over only after the driver's lease expires or is released.
//
// The lease is three columns beside the verdict: the owner, a token that
// starts at one and increments on every takeover, and the expiry.
// TakeOver is one conditional UPDATE that succeeds only when the stored
// expiry passed or the row already names the taker. Two processes that
// race for an expired task therefore see exactly one winner, and SQLite
// makes the statement atomic across processes sharing the file. Renew and
// ReleaseLease write only for the owner and token they name. Finish
// records the verdict and clears the lease only under the token it names.
// A leader that stalled past its lease therefore never overwrites the
// verdict the new owner recorded. The writes compare timestamps against the
// caller's now, so processes sharing a store must share a clock, or keep
// their skew well below the lease length.
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
	// rowToken is the fencing number the claim's first lease carries. A
	// takeover bumps it, so a write naming token zero can only come from a
	// row that never had a lease.
	rowToken = 1
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
// true. The inserted row carries no task id, StateRunning, and a lease
// naming owner with token one and the expiry now plus ttl. A row this call
// did not insert is returned unchanged, because a claim belongs to the Run
// that made it.
func (s *Store) Claim(ctx context.Context, key, owner string, now time.Time, ttl time.Duration) (providertask.Claim, bool, error) {
	if ttl <= 0 {
		return providertask.Claim{}, false, fmt.Errorf("sqlitestore: claim %s: %w: ttl must be positive", key, ErrInvalid)
	}
	tx, err := s.db.Writer().BeginTx(ctx, nil)
	if err != nil {
		return providertask.Claim{}, false, fmt.Errorf("sqlitestore: claim %s: %w", key, err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	result, err := tx.ExecContext(ctx,
		`INSERT INTO providertask_task (key, task_id, state, error_code, created_at, owner, token, lease_until)
			VALUES (?, ?, ?, '', ?, ?, ?, ?)
			ON CONFLICT (key) DO NOTHING`,
		key, rowTaskID, rowState, now.UnixNano(), owner, rowToken, now.Add(ttl).UnixNano())
	if err != nil {
		return providertask.Claim{}, false, fmt.Errorf("sqlitestore: claim %s: %w", key, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return providertask.Claim{}, false, fmt.Errorf("sqlitestore: claim %s: %w", key, err)
	}
	claim, err := scanClaim(tx.QueryRowContext(ctx,
		`SELECT key, task_id, state, error_code, created_at, owner, token, lease_until
			FROM providertask_task WHERE key = ?`, key))
	if err != nil {
		return providertask.Claim{}, false, fmt.Errorf("sqlitestore: claim %s: %w", key, err)
	}
	if err := tx.Commit(); err != nil {
		return providertask.Claim{}, false, fmt.Errorf("sqlitestore: claim %s: %w", key, err)
	}
	return claim, inserted == 1, nil
}

// TakeOver transfers the lease of key to owner when the stored expiry
// passed before now or the row already names owner. One conditional
// UPDATE decides it, so two processes that race for an expired task
// across one file see exactly one winner. The takeover bumps the token,
// so every write the previous holder still sends refuses as stale, and
// the new lease runs from now to now plus ttl. It reports ok false
// without changing the row when another driver still holds an unexpired
// lease. An unknown key reports an error matching
// providertask.ErrUnknownKey.
func (s *Store) TakeOver(ctx context.Context, key, owner string, now time.Time, ttl time.Duration) (int64, bool, error) {
	if ttl <= 0 {
		return 0, false, fmt.Errorf("sqlitestore: take over %s: %w: ttl must be positive", key, ErrInvalid)
	}
	tx, err := s.db.Writer().BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("sqlitestore: take over %s: %w", key, err)
	}
	defer func() { _ = tx.Rollback() }() // a committed transaction rolls back to a no-op
	result, err := tx.ExecContext(ctx,
		`UPDATE providertask_task
			SET owner = ?, token = token + 1, lease_until = ?
			WHERE key = ? AND (lease_until < ? OR owner = ?)`,
		owner, now.Add(ttl).UnixNano(), key, now.UnixNano(), owner)
	if err != nil {
		return 0, false, fmt.Errorf("sqlitestore: take over %s: %w", key, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("sqlitestore: take over %s: %w", key, err)
	}
	if updated == 0 {
		// A row that matched no lease window is either an unknown key or
		// one another driver still holds. The read tells the two apart.
		var one int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM providertask_task WHERE key = ?`, key).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, fmt.Errorf("sqlitestore: take over %s: %w", key, providertask.ErrUnknownKey)
		}
		if err != nil {
			return 0, false, fmt.Errorf("sqlitestore: take over %s: %w", key, err)
		}
		if err := tx.Commit(); err != nil {
			return 0, false, fmt.Errorf("sqlitestore: take over %s: %w", key, err)
		}
		return 0, false, nil
	}
	var token int64
	if err := tx.QueryRowContext(ctx,
		`SELECT token FROM providertask_task WHERE key = ?`, key).Scan(&token); err != nil {
		return 0, false, fmt.Errorf("sqlitestore: take over %s: %w", key, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("sqlitestore: take over %s: %w", key, err)
	}
	return token, true, nil
}

// Renew extends the lease of key to until when the row still names owner
// and carries token. It reports ok false without changing the row when
// they do not, which means the caller lost the drive and a takeover
// bumped the token underneath it.
func (s *Store) Renew(ctx context.Context, key, owner string, token int64, until time.Time) (bool, error) {
	result, err := s.db.Writer().ExecContext(ctx,
		`UPDATE providertask_task SET lease_until = ?
			WHERE key = ? AND owner = ? AND token = ?`,
		until.UnixNano(), key, owner, token)
	if err != nil {
		return false, fmt.Errorf("sqlitestore: renew %s: %w", key, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("sqlitestore: renew %s: %w", key, err)
	}
	return updated == 1, nil
}

// ReleaseLease clears the lease of key when the row still names owner and
// carries token, so the next Run takes over without waiting for expiry.
// The token bumps with the release, so a write the released driver still
// sends after it refuses as stale. It changes nothing when the owner or
// the token do not match, because the lease it names is no longer the
// row's to release.
func (s *Store) ReleaseLease(ctx context.Context, key, owner string, token int64) error {
	result, err := s.db.Writer().ExecContext(ctx,
		`UPDATE providertask_task
			SET owner = '', token = token + 1, lease_until = 0
			WHERE key = ? AND owner = ? AND token = ?`,
		key, owner, token)
	if err != nil {
		return fmt.Errorf("sqlitestore: release lease %s: %w", key, err)
	}
	if _, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("sqlitestore: release lease %s: %w", key, err)
	}
	return nil
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
		`SELECT key, task_id, state, error_code, created_at, owner, token, lease_until
			FROM providertask_task WHERE key = ?`, key))
	if err != nil {
		return providertask.Claim{}, unknownKey(key, err)
	}
	return claim, nil
}

// Finish records the provider's terminal verdict on the key's row and
// clears its lease. A state that names no terminal verdict reports
// ErrInvalid, and an unknown key reports an error matching
// providertask.ErrUnknownKey. A row that no longer carries token reports
// an error matching providertask.ErrStaleToken and changes nothing, so a
// driver that lost its lease never overwrites the verdict the new owner
// recorded.
func (s *Store) Finish(ctx context.Context, key string, token int64, state providertask.State, code string) error {
	if state != providertask.StateSucceeded && state != providertask.StateFailed {
		return fmt.Errorf("sqlitestore: finish %s: %w: state %q is not terminal", key, ErrInvalid, state)
	}
	result, err := s.db.Writer().ExecContext(ctx,
		`UPDATE providertask_task
			SET state = ?, error_code = ?, owner = '', lease_until = 0
			WHERE key = ? AND token = ?`,
		string(state), code, key, token)
	if err != nil {
		return fmt.Errorf("sqlitestore: finish %s: %w", key, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlitestore: finish %s: %w", key, err)
	}
	if updated == 0 {
		// A row that matched no token is either an unknown key or one
		// whose lease moved on. The read tells the two apart.
		var one int
		err := s.db.Reader().QueryRowContext(ctx,
			`SELECT 1 FROM providertask_task WHERE key = ?`, key).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlitestore: finish %s: %w", key, providertask.ErrUnknownKey)
		}
		if err != nil {
			return fmt.Errorf("sqlitestore: finish %s: %w", key, err)
		}
		return fmt.Errorf("sqlitestore: finish %s: %w", key, providertask.ErrStaleToken)
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
		claim      providertask.Claim
		taskID     string
		state      string
		errorCode  string
		createdAt  int64
		owner      string
		token      int64
		leaseUntil int64
	)
	if err := row.Scan(&claim.Key, &taskID, &state, &errorCode, &createdAt, &owner, &token, &leaseUntil); err != nil {
		return providertask.Claim{}, err
	}
	claim.TaskID = taskID
	claim.State = providertask.State(state)
	claim.Code = errorCode
	claim.CreatedAt = time.Unix(0, createdAt)
	claim.Owner = owner
	claim.Token = token
	// A stored zero is the no-lease state, so it reads as the zero time.
	if leaseUntil != 0 {
		claim.LeaseUntil = time.Unix(0, leaseUntil)
	}
	return claim, nil
}
