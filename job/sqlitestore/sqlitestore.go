// Package sqlitestore implements the job store over SQLite.
//
// The package owns one schema, applied through the sqlite package's
// migration runner, so the ledger records what ran and a second run changes
// nothing. Reads go through the read pool and every write through the single
// write connection, so concurrent jobs queue rather than race for the file
// lock.
package sqlitestore

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/nrynss/keel/job"
	"github.com/nrynss/keel/sqlite"
)

// schemaNamespace is the migration ledger namespace this package owns. It
// shares the database file with every other namespace without colliding,
// because each keeps its own ledger.
const schemaNamespace = "job"

//go:embed migrations/*.sql
var migrations embed.FS

// selectColumns is the column list every read shares, in the order
// scanRecord decodes it.
const selectColumns = `SELECT id, kind, status, attempt, parent_id, root_id, progress, result, error, updated_at FROM jobs`

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

// Store is the job store over one database. Create it with Open, because the
// zero value has no database. Store is safe for concurrent use.
type Store struct {
	db *sqlite.DB
}

// Open applies the job schema to cfg.DB and returns the store. The schema
// arrives as ordered SQL files, so the migration runner owns what ran and
// records each file once.
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

// Create stores rec. It records the job's identity, its lineage, its status
// and its latest progress snapshot, so a racing read finds the record.
func (s *Store) Create(ctx context.Context, rec job.Record) error {
	progress, err := encodeProgress(rec.Progress)
	if err != nil {
		return fmt.Errorf("sqlitestore: create %s: %w", rec.ID, err)
	}
	_, err = s.db.Writer().ExecContext(ctx,
		`INSERT INTO jobs (id, kind, status, attempt, parent_id, root_id, progress, result, error, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.Kind, string(rec.Status), rec.Attempt, rec.ParentID, rec.RootID,
		progress, rec.Data, errorText(rec.Err), rec.UpdatedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("sqlitestore: create %s: %w", rec.ID, err)
	}
	return nil
}

// Begin marks a queued record as running. It updates Status and UpdatedAt,
// called once when the attempt starts, and reports ErrUnknownJob for an id it
// does not hold.
func (s *Store) Begin(ctx context.Context, rec job.Record) error {
	return s.update(ctx, rec.ID,
		`UPDATE jobs SET status = ?, updated_at = ? WHERE id = ?`,
		string(rec.Status), rec.UpdatedAt.UnixNano(), rec.ID)
}

// SetProgress records the job's latest progress snapshot. It reads ID,
// Progress and UpdatedAt, and reports ErrUnknownJob for an id it does not
// hold.
func (s *Store) SetProgress(ctx context.Context, rec job.Record) error {
	progress, err := encodeProgress(rec.Progress)
	if err != nil {
		return fmt.Errorf("sqlitestore: set progress %s: %w", rec.ID, err)
	}
	return s.update(ctx, rec.ID,
		`UPDATE jobs SET progress = ?, updated_at = ? WHERE id = ?`,
		progress, rec.UpdatedAt.UnixNano(), rec.ID)
}

// Finish records the job's terminal state. It updates Status, Data, Err and
// UpdatedAt, keeps the Kind and lineage Create stored, and reports
// ErrUnknownJob for an id it does not hold.
func (s *Store) Finish(ctx context.Context, rec job.Record) error {
	return s.update(ctx, rec.ID,
		`UPDATE jobs SET status = ?, result = ?, error = ?, updated_at = ? WHERE id = ?`,
		string(rec.Status), rec.Data, errorText(rec.Err), rec.UpdatedAt.UnixNano(), rec.ID)
}

// Get returns the record stored under id. A missing row reports
// ErrUnknownJob, which a caller classifies with errors.Is.
func (s *Store) Get(ctx context.Context, id string) (job.Record, error) {
	return scanRecord(s.db.Reader().QueryRowContext(ctx, selectColumns+` WHERE id = ?`, id), "job "+id)
}

// Unfinished returns every record that has not reached a terminal state,
// oldest first, so a restart can interrupt a running record and queue a queued
// one again.
func (s *Store) Unfinished(ctx context.Context) ([]job.Record, error) {
	rows, err := s.db.Reader().QueryContext(ctx,
		selectColumns+` WHERE status IN (?, ?) ORDER BY updated_at, id`,
		string(job.StatusRunning), string(job.StatusQueued))
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: unfinished: %w", err)
	}
	return scanRecords(rows, "unfinished")
}

// Attempts returns every recorded attempt of the logical job id belongs to,
// oldest first. An unknown id reports ErrUnknownJob.
func (s *Store) Attempts(ctx context.Context, id string) ([]job.Record, error) {
	var root string
	err := s.db.Reader().QueryRowContext(ctx, `SELECT root_id FROM jobs WHERE id = ?`, id).Scan(&root)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("sqlitestore: attempts %s: %w", id, job.ErrUnknownJob)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: attempts %s: %w", id, err)
	}
	rows, err := s.db.Reader().QueryContext(ctx,
		selectColumns+` WHERE root_id = ? ORDER BY attempt, id`, root)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: attempts %s: %w", id, err)
	}
	return scanRecords(rows, "attempts "+id)
}

// update runs one write and classifies its row count. The rows affected
// count is what tells "updated" from "was never there", which no error from
// the driver distinguishes.
func (s *Store) update(ctx context.Context, id, query string, args ...any) error {
	res, err := s.db.Writer().ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("sqlitestore: update %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlitestore: update %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("sqlitestore: update %s: %w", id, job.ErrUnknownJob)
	}
	return nil
}

// rowScanner is the Scan subset shared by *sql.Row and *sql.Rows, so one
// decode serves the single-row and the listing read.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanRecords decodes every row of a listing read.
func scanRecords(rows *sql.Rows, what string) ([]job.Record, error) {
	defer rows.Close()
	var out []job.Record
	for rows.Next() {
		rec, err := scanRecord(rows, what)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlitestore: %s: %w", what, err)
	}
	return out, nil
}

// scanRecord decodes one row. This package wrote every column, so a decode
// failure other than a missing row is corruption and keeps its own cause.
func scanRecord(row rowScanner, what string) (job.Record, error) {
	var (
		rec       job.Record
		status    string
		progress  string
		errText   string
		updatedAt int64
	)
	err := row.Scan(&rec.ID, &rec.Kind, &status, &rec.Attempt, &rec.ParentID, &rec.RootID,
		&progress, &rec.Data, &errText, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return job.Record{}, fmt.Errorf("sqlitestore: %s: %w", what, job.ErrUnknownJob)
	}
	if err != nil {
		return job.Record{}, fmt.Errorf("sqlitestore: %s: %w", what, err)
	}
	rec.Status = job.Status(status)
	if err := decodeProgress(progress, &rec.Progress); err != nil {
		return job.Record{}, fmt.Errorf("sqlitestore: %s: %w", what, err)
	}
	if errText != "" {
		rec.Err = errors.New(errText)
	}
	rec.UpdatedAt = time.Unix(0, updatedAt)
	return rec, nil
}

// encodeProgress renders a snapshot as JSON text for its column.
func encodeProgress(p job.Progress) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("encode progress: %w", err)
	}
	return string(b), nil
}

// decodeProgress reads a snapshot back. An empty column means no snapshot
// was recorded, which is the zero Progress.
func decodeProgress(s string, p *job.Progress) error {
	if s == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(s), p); err != nil {
		return fmt.Errorf("decode progress: %w", err)
	}
	return nil
}

// errorText returns an error's message, or the empty string for a nil error.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
