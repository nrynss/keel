// Package sqlitestore implements the media index over SQLite.
//
// The package owns one schema, applied through the sqlite package's
// migration runner, so the ledger records what ran and a second run
// changes nothing. Every statement goes through the shared handle: reads
// on the read pool and every write on the write pool.
package sqlitestore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/nrynss/keel/mediastore"
	"github.com/nrynss/keel/sqlite"
)

// schemaNamespace is the migration ledger namespace this package owns.
// It shares the database file with every other namespace without
// colliding, because each keeps its own ledger.
const schemaNamespace = "mediastore"

//go:embed migrations/*.sql
var migrations embed.FS

// selectColumns is the column list every read shares, in the order
// scanMedia decodes it.
const selectColumns = `SELECT id, owner, media_group, content_type, size_bytes, visibility, created_at FROM media`

// ErrInvalid is returned for input this package cannot store: a nil
// database, an empty id, an empty content type, or a negative size.
var ErrInvalid = errors.New("sqlitestore: invalid row")

// Config configures Open.
type Config struct {
	// DB is the open database. It must not be nil, and this package
	// never closes it.
	DB *sqlite.DB
	// Namespace overrides the migration ledger namespace. Empty means
	// the one this package owns.
	Namespace string
}

// Store is the media index over one database. Create it with Open,
// because the zero value has no database. Store is safe for concurrent
// use.
type Store struct {
	db *sqlite.DB
}

// Open applies the media schema to cfg.DB and returns the index. The
// schema arrives as ordered SQL files, so the migration runner owns what
// ran and records each file once.
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
	return &Store{db: cfg.DB}, nil
}

// Create stores b. An id already in the table returns an error matching
// mediastore.ErrAlreadyExists, and a row this package cannot store
// returns one matching ErrInvalid.
func (s *Store) Create(ctx context.Context, b mediastore.Blob) error {
	if b.ID == "" {
		return fmt.Errorf("sqlitestore: create: %w: id must not be empty", ErrInvalid)
	}
	if b.ContentType == "" {
		return fmt.Errorf("sqlitestore: create: %w: content type must not be empty", ErrInvalid)
	}
	if b.SizeBytes < 0 {
		return fmt.Errorf("sqlitestore: create: %w: size %d must not be negative", ErrInvalid, b.SizeBytes)
	}
	// ON CONFLICT DO NOTHING keeps the duplicate an ordinary outcome
	// rather than a driver error to classify. No rows changed is the
	// signal, because the only constraint the schema declares is the
	// primary key.
	res, err := s.db.Writer().ExecContext(ctx,
		`INSERT INTO media (id, owner, media_group, content_type, size_bytes, visibility, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		b.ID, b.Owner, b.Group, b.ContentType, b.SizeBytes, visibilityValue(b.Visibility), b.CreatedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("sqlitestore: create media %s: %w", b.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlitestore: create media %s: rows affected: %w", b.ID, err)
	}
	if n == 0 {
		return fmt.Errorf("sqlitestore: create media %s: %w", b.ID, mediastore.ErrAlreadyExists)
	}
	return nil
}

// Get returns the blob stored under id. A missing row returns an error
// matching mediastore.ErrNotFound, which is the sentinel a mediastore
// caller classifies. Any other failure keeps its own cause, so a broken
// database never reads as a blob that is already gone.
func (s *Store) Get(ctx context.Context, id string) (mediastore.Blob, error) {
	return scanMedia(s.db.Reader().QueryRowContext(ctx, selectColumns+` WHERE id = ?`, id), "media "+id)
}

// Delete removes the row stored under id. A missing row returns an
// error matching mediastore.ErrNotFound.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.deleteWhere(ctx, `DELETE FROM media WHERE id = ?`, "media "+id, id)
}

// DeleteGroup removes every row in group. A group with no rows returns
// an error matching mediastore.ErrNotFound, and an empty group id is
// ErrInvalid, because an empty id names nothing to evict.
func (s *Store) DeleteGroup(ctx context.Context, group string) error {
	if group == "" {
		return fmt.Errorf("sqlitestore: delete group: %w: group must not be empty", ErrInvalid)
	}
	return s.deleteWhere(ctx, `DELETE FROM media WHERE media_group = ?`, "group "+group, group)
}

// deleteWhere runs one delete and classifies its row count. The rows
// affected count is what tells "removed" from "was never there", which
// no error from the driver distinguishes.
func (s *Store) deleteWhere(ctx context.Context, query, what string, arg any) error {
	res, err := s.db.Writer().ExecContext(ctx, query, arg)
	if err != nil {
		return fmt.Errorf("sqlitestore: delete %s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlitestore: delete %s: rows affected: %w", what, err)
	}
	if n == 0 {
		return fmt.Errorf("sqlitestore: delete %s: %w", what, mediastore.ErrNotFound)
	}
	return nil
}

// Groups returns every group with its blobs, ordered by group id. A
// blob with no group is unplaced and is not listed. Each group carries
// the creation time of its earliest blob.
func (s *Store) Groups(ctx context.Context) ([]mediastore.Group, error) {
	rows, err := s.db.Reader().QueryContext(ctx,
		selectColumns+` WHERE media_group <> '' ORDER BY media_group, created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: list groups: %w", err)
	}
	defer rows.Close()
	var groups []mediastore.Group
	for rows.Next() {
		b, err := scanMedia(rows, "grouped media")
		if err != nil {
			return nil, err
		}
		if len(groups) == 0 || groups[len(groups)-1].ID != b.Group {
			// The rows arrive ordered by group and then by creation, so
			// the first blob of a group is its earliest.
			groups = append(groups, mediastore.Group{ID: b.Group, CreatedAt: b.CreatedAt})
		}
		last := &groups[len(groups)-1]
		last.Blobs = append(last.Blobs, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlitestore: list groups: %w", err)
	}
	return groups, nil
}

// rowScanner is the Scan subset shared by *sql.Row and *sql.Rows, so one
// decode serves the single-row and the listing read.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanMedia decodes one row. This package wrote every column, so a
// decode failure other than a missing row is corruption and keeps its
// own cause.
func scanMedia(row rowScanner, what string) (mediastore.Blob, error) {
	var (
		b       mediastore.Blob
		vis     string
		created int64
	)
	if err := row.Scan(&b.ID, &b.Owner, &b.Group, &b.ContentType, &b.SizeBytes, &vis, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return mediastore.Blob{}, fmt.Errorf("sqlitestore: %s: %w", what, mediastore.ErrNotFound)
		}
		return mediastore.Blob{}, fmt.Errorf("sqlitestore: %s: %w", what, err)
	}
	if vis == string(mediastore.Public) {
		b.Visibility = mediastore.Public
	} else {
		// Anything else, an empty column included, is private. An
		// unrecognized visibility must never widen who may read.
		b.Visibility = mediastore.Private
	}
	b.CreatedAt = time.Unix(0, created).UTC()
	return b, nil
}

// visibilityValue maps the zero value to private, so a row is never
// stored with an empty visibility and the default stays deny.
func visibilityValue(v mediastore.Visibility) string {
	if v == mediastore.Public {
		return string(mediastore.Public)
	}
	return "private"
}
