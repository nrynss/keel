// Package sqlitestore keeps the multipart session records of direct
// uploads over SQLite.
//
// A session is one multipart upload an app opened for a client: the id
// the client holds, the upload id the service returned, the plan the
// parts follow, and the metadata the blob will carry at completion.
// The bucket holds the parts and nothing else, so the session record is
// the only state a restart or a sweep reads.
//
// The package owns one schema, applied through the sqlite package's
// migration runner under its own namespace, so the ledger records what
// ran and a second open changes nothing. Reads go through the read pool
// and every write through the single write connection, so concurrent
// creates queue rather than race for the file lock.
//
// Sweep abandons what the client left behind. A session older than the
// caller's bound is aborted in the bucket, which discards every part it
// holds, and its row is deleted once the abort went through. A row
// whose abort failed stays for the next pass. Never-completed objects
// are a different ownership: the bytes of a direct PUT that was never
// completed, and of a completion whose row write failed, age out under
// the store's orphan sweep by the backend's own modification time.
//
// This package sits on two edges. It is one of the few allowed to
// import the SQLite driver, and it compiles the object storage backend
// for one reason only, to pin the aborter to the backend's own shape,
// so a signature drift breaks the build here rather than at a call
// site.
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

	"github.com/nrynss/keel/mediastore"
	"github.com/nrynss/keel/mediastore/s3"
	"github.com/nrynss/keel/sqlite"
)

// schemaNamespace is the migration ledger namespace this package owns.
// It shares the database file with every other namespace without
// colliding, because each keeps its own ledger.
const schemaNamespace = "s3"

//go:embed migrations/*.sql
var migrations embed.FS

// ErrInvalid is returned for a session this package cannot store or a
// sweep it cannot run.
var ErrInvalid = errors.New("sqlitestore: invalid session")

// ErrNotFound is returned for a blob id the table holds no session
// for.
var ErrNotFound = errors.New("sqlitestore: no such session")

// ErrAlreadyExists is returned when a session for the blob id is
// already recorded.
var ErrAlreadyExists = errors.New("sqlitestore: session already exists")

// Aborter discards the parts of one multipart upload in the bucket.
// The object storage backend's AbortUpload satisfies it, and the pin
// below keeps the two shapes together.
type Aborter interface {
	AbortUpload(ctx context.Context, blobID, uploadID string) error
}

// The backend of this module is the aborter the sweep drives. A caller
// may supply any other implementation of the one method.
var _ Aborter = (*s3.Backend)(nil)

// Session is one multipart upload an app opened for a client. The
// metadata fields are what completion hands to the store when the
// parts have assembled, and the plan fields are what the part routes
// need while it runs.
type Session struct {
	// BlobID is the id the client holds and the key the object lands
	// under at completion.
	BlobID string
	// UploadID is the upload id the service returned for the session.
	UploadID string
	// Owner, Group and Visibility are the blob's metadata at
	// completion.
	Owner      string
	Group      string
	Visibility mediastore.Visibility
	// ContentType is the type the parts were opened for.
	ContentType string
	// SizeBytes is the whole-file size the client declared.
	SizeBytes int64
	// SHA256 is the whole-file digest the client declared, as 64 hex
	// characters. Completion checks the assembled object against it.
	SHA256 string
	// PartSize is the size every part carries except the last, which
	// carries whatever the whole-file size leaves over.
	PartSize int64
	// PartCount is the number of parts the plan divides the file into.
	PartCount int
	// CreatedAt is when the session was recorded.
	CreatedAt time.Time
}

// Config configures Open.
type Config struct {
	// DB is the open database the store writes through. It must not be
	// nil, and this package never closes it.
	DB *sqlite.DB
	// Namespace overrides the migration ledger namespace. Empty means
	// the one this package owns, which lets two stores share one file.
	Namespace string
	// Now supplies the clock the sweep ages sessions with. Nil means
	// time.Now.
	Now func() time.Time
	// Log receives one line per upload the sweep could not abort. Nil
	// discards.
	Log *slog.Logger
}

// Store keeps multipart session records over one database. Create it
// with Open, because the zero value has no database. Store is safe for
// concurrent use.
type Store struct {
	db  *sqlite.DB
	now func() time.Time
	log *slog.Logger
}

// Open applies the session schema to cfg.DB and returns the store. The
// schema arrives as ordered SQL files, so the migration runner owns
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
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Store{db: cfg.DB, now: now, log: log}, nil
}

// Create records a session. An id the table already holds is refused
// with an error matching ErrAlreadyExists, and a session this package
// cannot store is refused with one matching ErrInvalid.
func (s *Store) Create(ctx context.Context, sess Session) error {
	if sess.BlobID == "" || sess.UploadID == "" {
		return fmt.Errorf("sqlitestore: create %q: %w: the blob id and the upload id are required", sess.BlobID, ErrInvalid)
	}
	if sess.ContentType == "" {
		return fmt.Errorf("sqlitestore: create %q: %w: the content type is required", sess.BlobID, ErrInvalid)
	}
	if sess.SizeBytes < 0 {
		return fmt.Errorf("sqlitestore: create %q: %w: the size is negative", sess.BlobID, ErrInvalid)
	}
	if sess.SHA256 == "" {
		return fmt.Errorf("sqlitestore: create %q: %w: the digest is required", sess.BlobID, ErrInvalid)
	}
	if sess.PartSize < 1 {
		return fmt.Errorf("sqlitestore: create %q: %w: a part carries at least one byte", sess.BlobID, ErrInvalid)
	}
	if sess.PartCount < 1 || sess.PartCount > s3.MaxParts {
		return fmt.Errorf("sqlitestore: create %q: %w: %d parts is outside 1 to %d", sess.BlobID, ErrInvalid, sess.PartCount, s3.MaxParts)
	}
	// ON CONFLICT DO NOTHING keeps the duplicate an ordinary outcome
	// rather than a driver error to classify. No rows changed is the
	// signal, because the only constraint the schema declares is the
	// primary key.
	res, err := s.db.Writer().ExecContext(ctx,
		`INSERT INTO s3_multipart (blob_id, upload_id, owner, media_group, content_type, visibility, size_bytes, sha256, part_size, part_count, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(blob_id) DO NOTHING`,
		sess.BlobID, sess.UploadID, sess.Owner, sess.Group, sess.ContentType, string(sess.Visibility),
		sess.SizeBytes, sess.SHA256, sess.PartSize, sess.PartCount, sess.CreatedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("sqlitestore: create %q: %w", sess.BlobID, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlitestore: create %q: %w", sess.BlobID, err)
	}
	if inserted == 0 {
		return fmt.Errorf("sqlitestore: create %q: %w", sess.BlobID, ErrAlreadyExists)
	}
	return nil
}

// Get returns the session recorded for blobID, or an error matching
// ErrNotFound.
func (s *Store) Get(ctx context.Context, blobID string) (Session, error) {
	sess, err := scanSession(s.db.Reader().QueryRowContext(ctx,
		`SELECT blob_id, upload_id, owner, media_group, content_type, visibility, size_bytes, sha256, part_size, part_count, created_at
		 FROM s3_multipart WHERE blob_id = ?`, blobID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, fmt.Errorf("sqlitestore: get %q: %w", blobID, ErrNotFound)
		}
		return Session{}, fmt.Errorf("sqlitestore: get %q: %w", blobID, err)
	}
	return sess, nil
}

// Delete removes the session recorded for blobID, or returns an error
// matching ErrNotFound. Deleting a session whose upload still lives in
// the bucket leaves the parts behind, so a caller that abandons a
// session early aborts the upload first.
func (s *Store) Delete(ctx context.Context, blobID string) error {
	result, err := s.db.Writer().ExecContext(ctx,
		`DELETE FROM s3_multipart WHERE blob_id = ?`, blobID)
	if err != nil {
		return fmt.Errorf("sqlitestore: delete %q: %w", blobID, err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlitestore: delete %q: %w", blobID, err)
	}
	if removed == 0 {
		return fmt.Errorf("sqlitestore: delete %q: %w", blobID, ErrNotFound)
	}
	return nil
}

// Expired returns every session recorded before the given instant,
// oldest first. A caller that drives expiry itself walks this list and
// aborts each upload, and Sweep is the loop that does both.
func (s *Store) Expired(ctx context.Context, before time.Time) ([]Session, error) {
	rows, err := s.db.Reader().QueryContext(ctx,
		`SELECT blob_id, upload_id, owner, media_group, content_type, visibility, size_bytes, sha256, part_size, part_count, created_at
		 FROM s3_multipart WHERE created_at < ? ORDER BY created_at, blob_id`, before.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: expired: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlitestore: expired: %w", err)
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlitestore: expired: %w", err)
	}
	return out, nil
}

// Sweep aborts every session recorded older than olderThan and deletes
// its row, and returns the number of rows it removed. An upload the
// abort cannot discard is logged and left for the next pass, because
// removing the row would orphan parts the bucket still bills storage
// for. An upload the service no longer holds aborts as done, which is
// what a session a completion left behind reads as. A nil aborter is
// refused with an error matching ErrInvalid.
func (s *Store) Sweep(ctx context.Context, a Aborter, olderThan time.Duration) (int, error) {
	if a == nil {
		return 0, fmt.Errorf("sqlitestore: sweep: %w: no aborter", ErrInvalid)
	}
	expired, err := s.Expired(ctx, s.now().Add(-olderThan))
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: sweep: %w", err)
	}
	removed := 0
	for _, sess := range expired {
		if err := a.AbortUpload(ctx, sess.BlobID, sess.UploadID); err != nil {
			s.log.Error("sqlitestore: multipart upload not aborted", "blob_id", sess.BlobID, "err", err.Error())
			continue
		}
		if err := s.Delete(ctx, sess.BlobID); err != nil {
			return removed, fmt.Errorf("sqlitestore: sweep: %w", err)
		}
		removed++
	}
	return removed, nil
}

// rowScanner is the Scan subset shared by *sql.Row and *sql.Rows, so
// one decode serves the single-row and the listing read.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanSession decodes one session row. This package wrote every
// column, so a decode failure is corruption and keeps its own cause.
func scanSession(row rowScanner) (Session, error) {
	var (
		sess        Session
		visibility  string
		createdAtNs int64
	)
	if err := row.Scan(&sess.BlobID, &sess.UploadID, &sess.Owner, &sess.Group, &sess.ContentType, &visibility,
		&sess.SizeBytes, &sess.SHA256, &sess.PartSize, &sess.PartCount, &createdAtNs); err != nil {
		return Session{}, err
	}
	sess.Visibility = mediastore.Visibility(visibility)
	sess.CreatedAt = time.Unix(0, createdAtNs).UTC()
	return sess, nil
}
