// Package sqlitestore implements the cache entry index over SQLite.
//
// The package owns one schema, applied through the sqlite package's
// migration runner under its own namespace, so the ledger records what ran
// and a second open changes nothing. Reads go through the read pool and
// every write through the single write connection, so concurrent reads and
// writes queue rather than race for the file lock.
//
// An entry is one row keyed by the cache key the caller hashed. Put
// replaces the row under its key, so a refreshed generation takes the place
// of the one it replaces. The commit lands before Put returns, so a process
// that dies right after a make keeps the entry it paid for.
//
// Sweep deletes the rows time has expired and the rows whose mediastore
// blob a retention sweep has dropped. Run it on the same ticker as the
// mediastore sweeper, with Present wired to that index, so an entry and the
// blob its payload names leave together and the window where a hit names a
// dropped blob stays as short as the sweep interval.
//
// This package is one of the few allowed to import the SQLite driver. The
// expiry, the refusal policy and the single flight stay in the cache
// package.
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

	"github.com/nrynss/keel/cache"
	"github.com/nrynss/keel/sqlite"
)

// schemaNamespace is the migration ledger namespace this package owns. It
// shares the database file with every other namespace without colliding,
// because each namespace keeps its own ledger.
const schemaNamespace = "cache"

//go:embed migrations/*.sql
var migrations embed.FS

// ErrInvalid is returned by Open and Sweep for an argument they cannot
// honour, such as a nil database or a negative age.
var ErrInvalid = errors.New("sqlitestore: invalid config")

// Config configures Open.
type Config struct {
	// DB is the open database the store writes through. It must not be
	// nil, and this package never closes it.
	DB *sqlite.DB

	// Namespace overrides the migration ledger namespace. Empty means
	// the one this package owns, which lets two cache levels share one
	// file without sharing rows.
	Namespace string

	// Now supplies the clock a row with no stored time is stamped with
	// and the clock Sweep judges ages against. Nil means time.Now.
	Now func() time.Time

	// Logger receives the open record. Nil means slog.Default.
	Logger *slog.Logger
}

// Store keeps cache entries over one database. Create it with Open, because
// the zero value has no database. Store is safe for concurrent use, and
// satisfies cache.Store.
type Store struct {
	db  *sqlite.DB
	now func() time.Time
	log *slog.Logger
}

// Open applies the cache schema to cfg.DB and returns the store. The schema
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
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(ctx, "sqlitestore: opened cache entries", "namespace", namespace)
	return &Store{db: cfg.DB, now: now, log: logger}, nil
}

// Get returns the entry stored under key, or an error matching
// cache.ErrNotFound when the key has no row.
func (s *Store) Get(ctx context.Context, key string) (cache.Entry, error) {
	var (
		entry     cache.Entry
		refusal   int
		createdAt int64
	)
	err := s.db.Reader().QueryRowContext(ctx,
		`SELECT payload, content_type, charge_ref, blob_id, refusal, created_at
		 FROM cache_entry WHERE key = ?`, key,
	).Scan(&entry.Payload, &entry.ContentType, &entry.ChargeRef, &entry.BlobID, &refusal, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return cache.Entry{}, fmt.Errorf("sqlitestore: get %s: %w", key, cache.ErrNotFound)
	}
	if err != nil {
		return cache.Entry{}, fmt.Errorf("sqlitestore: get %s: %w", key, err)
	}
	entry.Refusal = refusal == 1
	entry.CreatedAt = time.Unix(0, createdAt).UTC()
	return entry, nil
}

// Put stores entry under key and replaces any row already there. A row with
// a zero CreatedAt is stamped with the store clock, because the cache
// package always sets one and a direct caller may not.
func (s *Store) Put(ctx context.Context, key string, entry cache.Entry) error {
	payload := entry.Payload
	if payload == nil {
		payload = []byte{}
	}
	created := entry.CreatedAt
	if created.IsZero() {
		created = s.now()
	}
	refusal := 0
	if entry.Refusal {
		refusal = 1
	}
	_, err := s.db.Writer().ExecContext(ctx,
		`INSERT INTO cache_entry (key, payload, content_type, charge_ref, blob_id, refusal, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET payload = excluded.payload,
			content_type = excluded.content_type,
			charge_ref = excluded.charge_ref,
			blob_id = excluded.blob_id,
			refusal = excluded.refusal,
			created_at = excluded.created_at`,
		key, payload, entry.ContentType, entry.ChargeRef, entry.BlobID, refusal, created.UTC().UnixNano())
	if err != nil {
		return fmt.Errorf("sqlitestore: put %s: %w", key, err)
	}
	return nil
}

// SweepConfig configures one Sweep pass. A zero age skips its class, so a
// caller that passes the resolved fields of its cache Config sweeps exactly
// what that Config expires.
type SweepConfig struct {
	// MaxAge is how old a non-refusal entry may be before the sweep
	// removes it. Zero skips the class.
	MaxAge time.Duration

	// RefusalTTL is how old a refusal entry may be before the sweep
	// removes it. Zero skips the class.
	RefusalTTL time.Duration

	// Present reports whether a blob id still resolves. Nil skips the
	// orphan class. Wire it to the mediastore index, so a row whose
	// blob a retention sweep has dropped leaves the index with it.
	Present func(ctx context.Context, blobID string) (bool, error)
}

// SweepResult is what one pass did. It is returned so a caller can log the
// classes of rows that went, the way the mediastore sweep reports its own.
type SweepResult struct {
	// ExpiredDeleted counts non-refusal rows past MaxAge.
	ExpiredDeleted int

	// RefusalsDeleted counts refusal rows past RefusalTTL.
	RefusalsDeleted int

	// OrphansDeleted counts rows whose blob no longer resolves.
	OrphansDeleted int
}

// Sweep runs one pass: expired entries, expired refusals, then rows whose
// blob is gone. A negative age is refused. It returns what it did.
func (s *Store) Sweep(ctx context.Context, cfg SweepConfig) (SweepResult, error) {
	if cfg.MaxAge < 0 || cfg.RefusalTTL < 0 {
		return SweepResult{}, fmt.Errorf("sqlitestore: sweep: %w: ages must not be negative", ErrInvalid)
	}
	now := s.now().UTC().UnixNano()
	var result SweepResult
	if cfg.MaxAge > 0 {
		n, err := s.deleteWhere(ctx, "refusal = 0 AND created_at <= ?", now-cfg.MaxAge.Nanoseconds())
		if err != nil {
			return SweepResult{}, err
		}
		result.ExpiredDeleted = n
	}
	if cfg.RefusalTTL > 0 {
		n, err := s.deleteWhere(ctx, "refusal = 1 AND created_at <= ?", now-cfg.RefusalTTL.Nanoseconds())
		if err != nil {
			return SweepResult{}, err
		}
		result.RefusalsDeleted = n
	}
	if cfg.Present != nil {
		n, err := s.deleteOrphans(ctx, cfg.Present)
		if err != nil {
			return result, err
		}
		result.OrphansDeleted = n
	}
	return result, nil
}

// deleteWhere removes the rows the where clause selects with one cutoff
// argument and counts them. The clause is one of this file's own constants,
// never caller text.
func (s *Store) deleteWhere(ctx context.Context, where string, cutoff int64) (int, error) {
	res, err := s.db.Writer().ExecContext(ctx, `DELETE FROM cache_entry WHERE `+where, cutoff)
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: sweep: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: sweep: %w", err)
	}
	return int(n), nil
}

// blobRow is one entry that names a blob, read for the orphan pass.
type blobRow struct {
	key    string
	blobID string
}

// deleteOrphans reads every row that names a blob and deletes the ones
// whose blob no longer resolves. The read and the deletes do not share one
// transaction, because a row removed under this pass deletes nothing the
// second time.
func (s *Store) deleteOrphans(ctx context.Context, present func(context.Context, string) (bool, error)) (int, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT key, blob_id FROM cache_entry WHERE blob_id != ''`)
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: sweep: list blob rows: %w", err)
	}
	defer rows.Close() // the cursor is drained before the caller returns
	var candidates []blobRow
	for rows.Next() {
		var row blobRow
		if err := rows.Scan(&row.key, &row.blobID); err != nil {
			return 0, fmt.Errorf("sqlitestore: sweep: %w", err)
		}
		candidates = append(candidates, row)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("sqlitestore: sweep: list blob rows: %w", err)
	}
	deleted := 0
	for _, row := range candidates {
		kept, err := present(ctx, row.blobID)
		if err != nil {
			return deleted, fmt.Errorf("sqlitestore: sweep: present %s: %w", row.blobID, err)
		}
		if kept {
			continue
		}
		if _, err := s.db.Writer().ExecContext(ctx, `DELETE FROM cache_entry WHERE key = ?`, row.key); err != nil {
			return deleted, fmt.Errorf("sqlitestore: sweep: delete orphan %s: %w", row.key, err)
		}
		deleted++
	}
	return deleted, nil
}
