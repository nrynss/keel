// Package sqlitestore keeps lease rows in SQLite.
// The package owns one schema, applied through the sqlite migration runner
// under its own namespace, so a second open changes nothing. Expiry is a
// comparison against the stored deadline on every read, so no process must
// stay alive for a cap to hold, and the fact survives a restart on disk.
// A cancelled context stops a call before it commits. The call fails and
// stores nothing, so a caller may retry it, and a read returns no result
// rather than the rows it had already decoded.
// This package is one of the few allowed to import the SQLite driver. All
// lease arithmetic stays in the lease package.
package sqlitestore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/nrynss/keel/cost"
	"github.com/nrynss/keel/lease"
	"github.com/nrynss/keel/sqlite"
)

// schemaNamespace is the migration ledger namespace this package owns. It
// shares the database file with every other namespace without colliding,
// because each namespace keeps its own ledger.
const schemaNamespace = "lease"

//go:embed migrations/*.sql
var migrations embed.FS

// ErrInvalid is returned for an argument this package cannot honour, such
// as a nil database.
var ErrInvalid = errors.New("sqlitestore: invalid config")

// Config configures Open.
type Config struct {
	// DB is the open database. It must not be nil, and this package never
	// closes it.
	DB *sqlite.DB

	// Namespace overrides the migration ledger namespace. Empty means the
	// one this package owns.
	Namespace string

	// Logger receives the open record. Nil means slog.Default.
	Logger *slog.Logger
}

// Store keeps lease rows over one database. Create it with Open, because
// the zero value has no database. Store is safe for concurrent use, and
// satisfies lease.Store.
type Store struct {
	db  *sqlite.DB
	log *slog.Logger
}

// Open applies the lease schema to cfg.DB and returns the store. A
// cancelled context stops the open and no store comes back, but the schema
// and its migration ledger row may already have landed. Those bytes are
// inert, so a caller may retry the open and a second open on the same file
// is unaffected.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("sqlitestore: open: %w: DB must not be nil", ErrInvalid)
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
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(ctx, "sqlitestore: opened leases", "namespace", namespace)
	return &Store{db: cfg.DB, log: logger}, nil
}

// Create writes a fresh open lease row. It reports an error matching
// lease.ErrInvalid when the lease carries no id, because a row with no
// key is never what a caller means.
func (s *Store) Create(ctx context.Context, l lease.Lease) error {
	if l.ID == "" {
		return fmt.Errorf("sqlitestore: create: %w: id must not be empty", lease.ErrInvalid)
	}
	_, err := s.db.Writer().ExecContext(ctx,
		`INSERT INTO lease_entry (id, state, opened_at, expires_at, closed_at,
			estimate_nd, settled_nd, reported_nd, reconciled, kind, owner)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ID, string(l.State), l.OpenedAt.UnixNano(), l.ExpiresAt.UnixNano(),
		unixOrZero(l.ClosedAt), int64(l.Estimate), int64(l.Settled),
		int64(l.Reported), boolToInt(l.Reconciled), l.Kind, l.Owner)
	if err != nil {
		return fmt.Errorf("sqlitestore: create lease %s: %w", l.ID, err)
	}
	return nil
}

// Get reads the lease stored under id. It returns the stored row unchanged.
// The manager judges expiry from the stored deadline on every touch, so the
// fact holds with no process involved and survives a restart on disk. An
// unknown id reports an error matching lease.ErrUnknownLease.
func (s *Store) Get(ctx context.Context, id string) (lease.Lease, error) {
	row := s.db.Reader().QueryRowContext(ctx,
		`SELECT id, state, opened_at, expires_at, closed_at,
			estimate_nd, settled_nd, reported_nd, reconciled, kind, owner
		FROM lease_entry WHERE id = ?`, id)
	l, err := scanLease(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return lease.Lease{}, fmt.Errorf("sqlitestore: get lease %s: %w", id, lease.ErrUnknownLease)
		}
		return lease.Lease{}, fmt.Errorf("sqlitestore: get lease %s: %w", id, err)
	}
	return l, nil
}

// Update writes back the lease stored under id. An unknown id reports an
// error matching lease.ErrUnknownLease. A cancelled context stops the
// write, so the old row stays.
func (s *Store) Update(ctx context.Context, l lease.Lease) error {
	if l.ID == "" {
		return fmt.Errorf("sqlitestore: update: %w: id must not be empty", lease.ErrInvalid)
	}
	result, err := s.db.Writer().ExecContext(ctx,
		`UPDATE lease_entry SET state = ?, opened_at = ?, expires_at = ?,
			closed_at = ?, estimate_nd = ?, settled_nd = ?,
			reported_nd = ?, reconciled = ?, kind = ?, owner = ?
		WHERE id = ?`,
		string(l.State), l.OpenedAt.UnixNano(), l.ExpiresAt.UnixNano(),
		unixOrZero(l.ClosedAt), int64(l.Estimate), int64(l.Settled),
		int64(l.Reported), boolToInt(l.Reconciled), l.Kind, l.Owner, l.ID)
	if err != nil {
		return fmt.Errorf("sqlitestore: update lease %s: %w", l.ID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlitestore: update lease %s: %w", l.ID, err)
	}
	if affected == 0 {
		return fmt.Errorf("sqlitestore: update lease %s: %w", l.ID, lease.ErrUnknownLease)
	}
	return nil
}

// CloseIfOpen writes back lease only when the stored row still reads open.
// It reports true when it wrote the closed row. It reports false with no
// error when the row already reads closed or expired, and the stored row
// stays as the winner left it. An unknown id reports an error matching
// lease.ErrUnknownLease. The compare and the write share one statement, so
// two processes closing one lease leave exactly one winner.
func (s *Store) CloseIfOpen(ctx context.Context, l lease.Lease) (bool, error) {
	if l.ID == "" {
		return false, fmt.Errorf("sqlitestore: close: %w: id must not be empty", lease.ErrInvalid)
	}
	result, err := s.db.Writer().ExecContext(ctx,
		`UPDATE lease_entry SET state = ?, opened_at = ?, expires_at = ?,
			closed_at = ?, estimate_nd = ?, settled_nd = ?,
			reported_nd = ?, reconciled = ?, kind = ?, owner = ?
		WHERE id = ? AND state = ?`,
		string(l.State), l.OpenedAt.UnixNano(), l.ExpiresAt.UnixNano(),
		unixOrZero(l.ClosedAt), int64(l.Estimate), int64(l.Settled),
		int64(l.Reported), boolToInt(l.Reconciled), l.Kind, l.Owner, l.ID, string(lease.StateOpen))
	if err != nil {
		return false, fmt.Errorf("sqlitestore: close lease %s: %w", l.ID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("sqlitestore: close lease %s: %w", l.ID, err)
	}
	if affected == 0 {
		_, err := s.Get(ctx, l.ID)
		if err != nil {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

// ExpiredOpen lists every row that still reads open or closing with a
// deadline at or before now, oldest deadline first. Closing rows land here
// because a closer may die between the claim write and the final write, and
// the stranded row must expire with the ordinary open ones. The limit
// bounds the pass, so one call never loads the whole table. A cancelled
// context stops the read and returns no rows rather than the rows it had
// already decoded.
func (s *Store) ExpiredOpen(ctx context.Context, now time.Time, limit int) ([]lease.Lease, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("sqlitestore: expired open: %w: limit must be positive", lease.ErrInvalid)
	}
	rows, err := s.db.Reader().QueryContext(ctx,
		`SELECT id, state, opened_at, expires_at, closed_at,
			estimate_nd, settled_nd, reported_nd, reconciled, kind, owner
		FROM lease_entry WHERE state IN (?, ?) AND expires_at <= ? ORDER BY expires_at, id LIMIT ?`,
		string(lease.StateOpen), string(lease.StateClosing), now.UnixNano(), limit)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: expired open: %w", err)
	}
	defer rows.Close()
	var out []lease.Lease
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlitestore: expired open: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlitestore: expired open: %w", err)
	}
	return out, nil
}

// scanner is the row shape Get decodes, so a row and its scan stay in one
// place.
type scanner interface {
	Scan(dest ...any) error
}

// scanLease decodes one lease row.
func scanLease(row scanner) (lease.Lease, error) {
	var l lease.Lease
	var state string
	var opened, expires, closed int64
	var estimate, settled, reported int64
	var reconciled int
	if err := row.Scan(&l.ID, &state, &opened, &expires, &closed,
		&estimate, &settled, &reported, &reconciled, &l.Kind, &l.Owner); err != nil {
		return lease.Lease{}, err
	}
	l.State = lease.State(state)
	l.OpenedAt = time.Unix(0, opened).UTC()
	l.ExpiresAt = time.Unix(0, expires).UTC()
	if closed != 0 {
		l.ClosedAt = time.Unix(0, closed).UTC()
	}
	l.Estimate = cost.Price(estimate)
	l.Settled = cost.Price(settled)
	l.Reported = cost.Price(reported)
	l.Reconciled = reconciled != 0
	return l, nil
}

// unixOrZero renders t as nanoseconds, or zero while unset, so an open
// lease stores no close time.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// boolToInt renders b for the reconciled column.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
