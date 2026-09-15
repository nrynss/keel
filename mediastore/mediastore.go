// Package mediastore persists media bytes on disk and serves them back
// over HTTP.
//
// A store owns one directory and one metadata index. Each blob is one
// file named by its id, opened and created through an os.Root confined
// to that directory. The metadata row lives behind the BlobIndex
// interface, which this package declares and the caller supplies.
//
// Store is an http.Handler. Register it at "GET /media/{id}". It
// answers through http.ServeContent, so a Range request returns 206
// with exactly the requested bytes. The content types it stores are a
// closed set, because the handler serves the stored type verbatim.
//
// Configuration arrives through Config, and the package reads no
// environment. Errors are sentinels matched with errors.Is. Every
// Persist that returns without error survives a process restart,
// because the bytes are fsynced before the row is written. A Persist
// that fails removes only the file it created, so bytes already stored
// under an id are never truncated or removed.
package mediastore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nrynss/keel/id"
)

// ErrNotFound is returned when an id has no stored blob: an unknown
// id, a malformed one, or a row whose file has vanished. The index
// wraps it for a missing row, so one sentinel covers every absent-blob
// shape and a caller matches it with errors.Is. A server fault, such as
// a cancelled context, never matches it, so "already gone" and "broken
// now" stay distinguishable.
var ErrNotFound = errors.New("not found")

// ErrAlreadyExists is returned by a BlobIndex that is asked to create a
// row for an id it already holds, and by PersistWithID when the id's
// name is already occupied on disk. The interface documents it, so a
// caller of PersistWithID can classify a taken id.
var ErrAlreadyExists = errors.New("mediastore: id already exists")

// ErrInvalidContentType is returned by Persist for an empty or
// unsupported content type. The set is closed on purpose, because the
// handler serves the stored type verbatim, so bytes with an unvetted
// type must never reach disk.
var ErrInvalidContentType = errors.New("mediastore: invalid content type")

// ErrInvalid is returned for unusable configuration: a missing
// directory, a missing index, or a content type entry that normalizes
// to nothing.
var ErrInvalid = errors.New("mediastore: invalid config")

// defaultContentTypes is the closed set Persist accepts when Config
// leaves ContentTypes empty. It covers the image types an image model
// returns, the audio types a speech model returns alongside Ogg and
// WebM containers, the subtitle tracks a captioner writes for those
// containers, video, and the printable PDF.
var defaultContentTypes = []string{
	"image/png",
	"image/jpeg",
	"image/webp",
	"audio/mpeg",
	"audio/wav",
	"audio/ogg",
	"audio/webm",
	"video/mp4",
	"application/pdf",
	"text/vtt",
	"application/x-subrip",
}

// Visibility says who may read a blob.
type Visibility string

// Private is a blob only Config.Authorize may serve. It is the zero
// value, so a Blob that does not set Visibility is private and the
// default answer to "who may read this" is "not everyone".
const Private Visibility = ""

// Public is a blob anyone may read. It is served with a one-year
// immutable cache header, because its id names exactly these bytes.
const Public Visibility = "public"

// Blob is the metadata of one stored blob. The bytes live in the
// store's directory under a file named by ID.
type Blob struct {
	// ID is the blob's name on disk and in the index. It comes from the
	// id package, which makes it unguessable.
	ID string
	// Owner is the caller's name for whoever owns the blob. The store
	// never interprets it.
	Owner string
	// Group is the eviction unit the blob belongs to. A group goes as a
	// whole, because half a group is worse than none of it. An empty
	// group leaves the blob unplaced and the retention sweep removes it
	// by age.
	Group string
	// ContentType is the type the blob is served as, already normalized.
	ContentType string
	// SizeBytes is the blob's size on disk.
	SizeBytes int64
	// Visibility is who may read the blob. The zero value is private.
	Visibility Visibility
	// CreatedAt is when the store accepted the blob.
	CreatedAt time.Time
}

// Group is one eviction unit: every blob that shares a group id, and
// the time the group first appeared. The retention sweep evicts a group
// as a unit and never splits one.
type Group struct {
	// ID is the group id shared by Blobs.
	ID string
	// CreatedAt is the creation time of the group's earliest blob.
	CreatedAt time.Time
	// Blobs holds every blob in the group, earliest first.
	Blobs []Blob
}

// BlobIndex stores blob metadata. Implementations live in their own
// package, which is why this package declares the interface and never
// the storage. Implementations must be safe for concurrent use, must
// return an error matching ErrNotFound for a missing row, and may
// return an error matching ErrAlreadyExists when an id is taken.
//
// Get and Delete must never return ErrNotFound for a fault. A closed
// database or a cancelled context is a different error, so a server
// fault can never be mistaken for a blob that is already gone.
type BlobIndex interface {
	// Create stores b. It returns ErrAlreadyExists when the id is taken.
	Create(ctx context.Context, b Blob) error
	// Get returns the blob stored under id, or ErrNotFound.
	Get(ctx context.Context, id string) (Blob, error)
	// Delete removes the blob stored under id, or returns ErrNotFound.
	Delete(ctx context.Context, id string) error
	// DeleteGroup removes every blob in group, or returns ErrNotFound.
	DeleteGroup(ctx context.Context, group string) error
	// Groups lists every group with its blobs.
	Groups(ctx context.Context) ([]Group, error)
}

// Put describes a blob before its bytes are written.
type Put struct {
	// ContentType is the type the blob is served as. It must normalize
	// to one of the store's accepted types.
	ContentType string
	// Owner is the caller's name for the blob's owner.
	Owner string
	// Group is the eviction unit the blob belongs to. An empty group
	// leaves the blob unplaced.
	Group string
	// Visibility is who may read the blob. The zero value is private.
	Visibility Visibility
}

// Config configures Open.
type Config struct {
	// Dir is the directory blob files are written under. Open creates
	// it when it is absent.
	Dir string
	// Index stores the metadata rows. It must not be nil.
	Index BlobIndex
	// Log receives one line per server fault, such as a row whose file
	// cannot be opened. Nil discards.
	Log *slog.Logger
	// Now is the clock the store stamps CreatedAt from. Nil means
	// time.Now.
	Now func() time.Time
	// ContentTypes is the set Persist accepts. Nil or empty means the
	// default set of image, audio, video and subtitle types. Each entry
	// normalizes the same way a stored type does, and an entry that
	// normalizes to nothing is ErrInvalid.
	ContentTypes []string
	// Authorize reports whether a request may read a private blob. It
	// receives the live request and the blob's metadata. Nil refuses
	// every private blob, so a store with no authorizer serves public
	// blobs only.
	Authorize func(*http.Request, Blob) bool
}

// Store persists blobs on disk, records their metadata through a
// BlobIndex, and serves them over HTTP. Create it with Open, because
// the zero value has no directory, no index and no clock. Store is safe
// for concurrent use.
type Store struct {
	dir       string
	root      *os.Root
	index     BlobIndex
	log       *slog.Logger
	now       func() time.Time
	types     map[string]bool
	authorize func(*http.Request, Blob) bool

	// newBlob exclusively creates the blob file named id, so it never
	// opens an existing file for truncation. Open binds it to the root
	// handle. A test substitutes a failing handle to reach writeBlob's
	// sync and close failure branches, which no real disk produces on
	// demand.
	newBlob func(id string) (blobFile, error)
}

// Open creates the blob directory when it is absent, resolves the
// configuration, and returns the Store. The directory handle is an
// os.Root, so every blob open and create is confined to Dir even if a
// crafted id ever reached it.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.Dir == "" {
		return nil, fmt.Errorf("mediastore: open: %w: Dir must not be empty", ErrInvalid)
	}
	if cfg.Index == nil {
		return nil, fmt.Errorf("mediastore: open: %w: Index must not be nil", ErrInvalid)
	}
	types, err := buildTypes(cfg.ContentTypes)
	if err != nil {
		return nil, fmt.Errorf("mediastore: open: %w", err)
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("mediastore: open: create dir: %w", err)
	}
	root, err := os.OpenRoot(cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("mediastore: open: %w", err)
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	s := &Store{
		dir:       cfg.Dir,
		root:      root,
		index:     cfg.Index,
		log:       log,
		now:       now,
		types:     types,
		authorize: cfg.Authorize,
	}
	s.newBlob = func(blobID string) (blobFile, error) {
		// An exclusive create never truncates a file already stored
		// under the id, so a taken name fails with fs.ErrExist and this
		// call leaves the existing bytes alone.
		return s.root.OpenFile(blobID, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	}
	return s, nil
}

// buildTypes resolves Config.ContentTypes into the accepted set. A
// missing list means the default set, and every entry is normalized the
// same way a stored content type is, so a caller can write the set the
// way a header spells it.
func buildTypes(list []string) (map[string]bool, error) {
	if len(list) == 0 {
		list = defaultContentTypes
	}
	types := make(map[string]bool, len(list))
	for _, entry := range list {
		bare := bareType(entry)
		if bare == "" {
			return nil, fmt.Errorf("%w: content type %q normalizes to nothing", ErrInvalid, entry)
		}
		types[bare] = true
	}
	return types, nil
}

// bareType cuts a content type's parameters, trims whitespace and
// lowercases what remains.
func bareType(contentType string) string {
	bare, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(bare))
}

// Persist writes src as a new blob and returns its unguessable id.
//
// The content type must be one of the store's accepted types.
// Anything else, including empty, is ErrInvalidContentType. A type with
// parameters such as "image/png; charset=binary" is accepted and stored
// as its bare media type. The blob is fsynced before the metadata row
// is inserted, so a Persist that returns without error survives a
// process restart. The copy reads only from src, and ctx bounds the
// metadata write.
func (s *Store) Persist(ctx context.Context, src io.Reader, p Put) (string, error) {
	ct, ok := s.normalizeContentType(p.ContentType)
	if !ok {
		return "", fmt.Errorf("mediastore: persist: %w: %q", ErrInvalidContentType, p.ContentType)
	}
	blobID, err := id.New()
	if err != nil {
		return "", fmt.Errorf("mediastore: persist: %w", err)
	}
	p.ContentType = ct
	return s.persistWithID(ctx, blobID, src, p)
}

// PersistWithID is the caller supplied id variant of Persist. It lets a
// consumer reserve the id before the blob and its row become reachable,
// so a separate ownership record can cover an interrupted persist. A
// chunked upload assembles its parts and lands them through here. The
// id must have the shape the id package produces, and anything else is
// ErrNotFound, the same answer an unknown id gets.
//
// An id whose row the index already holds is refused with
// ErrAlreadyExists, and so is one whose name is already occupied on
// disk. The file is created exclusively, so a call refused this way
// writes nothing and the blob stored under the id keeps its bytes.
func (s *Store) PersistWithID(ctx context.Context, blobID string, src io.Reader, p Put) error {
	ct, ok := s.normalizeContentType(p.ContentType)
	if !ok {
		return fmt.Errorf("mediastore: persist: %w: %q", ErrInvalidContentType, p.ContentType)
	}
	if !id.Valid(blobID) {
		return fmt.Errorf("mediastore: persist: %w: malformed id", ErrNotFound)
	}
	p.ContentType = ct
	_, err := s.persistWithID(ctx, blobID, src, p)
	return err
}

// persistWithID writes the bytes, then the row. The order matters in
// both directions: the row is what makes the blob reachable, so the
// bytes must be durable first, and a row that cannot be written takes
// the file this call created with it. The bytes are created
// exclusively, so a name already on disk is refused before anything is
// written and the stored bytes are never touched.
func (s *Store) persistWithID(ctx context.Context, blobID string, src io.Reader, p Put) (string, error) {
	size, err := s.writeBlob(blobID, src)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			// writeBlob owns nothing when its exclusive create fails,
			// so this call removes nothing and the stored bytes stand.
			return "", fmt.Errorf("mediastore: persist %s: %w: %w", blobID, ErrAlreadyExists, err)
		}
		return "", fmt.Errorf("mediastore: persist: %w", err)
	}
	b := Blob{
		ID:          blobID,
		Owner:       p.Owner,
		Group:       p.Group,
		ContentType: p.ContentType,
		SizeBytes:   size,
		Visibility:  p.Visibility,
		CreatedAt:   s.now().UTC(),
	}
	if err := s.index.Create(ctx, b); err != nil {
		// The exclusive create succeeded, so the file belongs to this
		// call. No row means the blob is unreachable through the API,
		// so the file is removed rather than left to leak disk space.
		if rmErr := s.root.Remove(blobID); rmErr != nil {
			s.log.Error("mediastore: orphaned blob after failed insert", "id", blobID, "err", rmErr.Error())
		}
		return "", fmt.Errorf("mediastore: persist: %w", err)
	}
	return blobID, nil
}

// blobFile is the file handle writeBlob writes through: io.Copy's
// Write, the Sync that puts the bytes on disk before the row lands, and
// the Close. *os.File is the production implementation, and the seam
// exists so the never-expected sync and close failure branches can be
// injected in a test.
type blobFile interface {
	io.WriteCloser
	Sync() error
}

// writeBlob copies src into a file created exclusively for id and
// fsyncs before close, so the bytes are on disk when Persist returns.
// The exclusive create never opens an existing file for truncation, so
// a name already on disk reports fs.ErrExist and this call writes
// nothing. Every failure after the create removes the file this call
// owns, because a blob that was never inserted must not leak disk
// space.
func (s *Store) writeBlob(blobID string, src io.Reader) (int64, error) {
	f, err := s.newBlob(blobID)
	if err != nil {
		// A failed create owns no file, so nothing is removed here and
		// an existing file keeps its bytes.
		return 0, fmt.Errorf("create blob: %w", err)
	}
	// fail gives up on the blob: nothing it wrote is reachable, because
	// the row comes later, so the file this call created is removed
	// before returning. The close here is best effort, since the file's
	// next stop is Remove and a close error on a failing write changes
	// nothing about that.
	fail := func(stage string, err error) (int64, error) {
		f.Close() // best effort, the file is about to be removed anyway
		if rmErr := s.root.Remove(blobID); rmErr != nil {
			s.log.Error("mediastore: partial blob not removed", "id", blobID, "stage", stage, "err", rmErr.Error())
		}
		return 0, fmt.Errorf("%s blob: %w", stage, err)
	}
	size, err := io.Copy(f, src)
	if err != nil {
		return fail("write", err)
	}
	if err := f.Sync(); err != nil {
		return fail("sync", err)
	}
	if err := f.Close(); err != nil {
		return fail("close", err)
	}
	return size, nil
}

// Delete removes the blob's metadata row and then its file. The row
// goes first, so a crash between the two leaves an unreferenced file,
// which is the shape the retention sweep removes by age. An unknown or
// malformed id returns an error matching ErrNotFound.
func (s *Store) Delete(ctx context.Context, blobID string) error {
	if err := s.index.Delete(ctx, blobID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return notFound(blobID, fmt.Errorf("delete: %w", err))
		}
		return fmt.Errorf("mediastore: delete: %w", err)
	}
	// The file is already unreachable once the row is gone, so a
	// failure here is a log line and not a caller error.
	if err := s.root.Remove(blobID); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.log.Error("mediastore: unreferenced blob not removed", "id", blobID, "err", err.Error())
	}
	return nil
}

// DeleteIfPresent removes a blob when it exists and succeeds when the
// id is already absent. A caller that reserved an id before its row
// existed needs the second case, because a crash may land between the
// two.
func (s *Store) DeleteIfPresent(ctx context.Context, blobID string) error {
	if err := s.Delete(ctx, blobID); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

// notFound classifies err as ErrNotFound for id while keeping err in
// the chain, so a consumer that follows either the index's contract or
// this package's matches with errors.Is.
func notFound(id string, err error) error {
	return fmt.Errorf("mediastore: media %s: %w: %w", id, ErrNotFound, err)
}

// find resolves one id to its metadata row, which is the lookup
// ServeHTTP and the retention sweep classify on. Every absent-blob
// shape matches ErrNotFound, and any other failure matches no sentinel,
// so "already gone" can never be confused with a server fault.
func (s *Store) find(ctx context.Context, blobID string) (Blob, error) {
	if !id.Valid(blobID) {
		return Blob{}, fmt.Errorf("mediastore: media %q: %w: malformed id", blobID, ErrNotFound)
	}
	b, err := s.index.Get(ctx, blobID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Blob{}, notFound(blobID, err)
		}
		return Blob{}, fmt.Errorf("mediastore: media %s: %w", blobID, err)
	}
	return b, nil
}

// ServeHTTP serves one stored blob at GET or HEAD /media/{id}.
// http.ServeContent does the protocol work, so a Range request returns
// 206, a matching ETag returns 304, and HEAD sends no body.
//
// An unknown, malformed or file-less id is 404. A blob that exists but
// cannot be opened is 500 and one log line. A private blob is served
// only when Config.Authorize allows the request, and a refusal answers
// the same 404 as an unknown id, so the response never confirms that a
// private blob exists.
func (s *Store) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	blobID := r.PathValue("id")
	b, err := s.find(r.Context(), blobID)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.log.Error("mediastore: metadata lookup failed", "id", blobID, "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if b.Visibility != Public && (s.authorize == nil || !s.authorize(r, b)) {
		// A refusal is deliberately identical to an unknown id. The
		// answer must not tell a caller whether the blob exists.
		http.NotFound(w, r)
		return
	}
	f, err := s.root.Open(blobID)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The row says the blob exists and the file says otherwise,
			// which is the vanished-file shape. The client sees the same
			// 404 as an unknown id and the operator sees the classified
			// error.
			s.log.Error("mediastore: row without file", "id", blobID, "err", notFound(blobID, err).Error())
			http.NotFound(w, r)
			return
		}
		s.log.Error("mediastore: blob open failed", "id", blobID, "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", b.ContentType)
	if b.Visibility == Public {
		// A public blob's id names exactly these bytes for good, so an
		// edge cache may hold the body for a year and never revalidate.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		// A private blob may be readable only through a short-lived
		// grant, so nothing between here and the reader keeps a copy.
		w.Header().Set("Cache-Control", "private, no-store")
	}
	w.Header().Set("ETag", `"`+b.ID+`"`)
	// No modtime, because the bytes are immutable, so a conditional
	// request rides on the ETag alone.
	http.ServeContent(w, r, "", time.Time{}, f)
}

// normalizeContentType cuts parameters, trims case and whitespace, and
// reports whether what remains is one of the store's accepted types.
func (s *Store) normalizeContentType(contentType string) (string, bool) {
	bare := bareType(contentType)
	if bare == "" || !s.types[bare] {
		return "", false
	}
	return bare, true
}
