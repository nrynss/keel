package mediastore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"
)

// BlobReader reads the stored bytes of one blob. Seek is part of the
// shape, because the HTTP handler answers Range requests through
// http.ServeContent, and a capture or a copy reads the bytes whole
// through the same handle.
type BlobReader interface {
	io.ReadSeeker
	io.Closer
}

// Object is one stored object as the backend holds it, without its
// metadata row. A backend lists what it holds, and the store classifies
// each object against the index.
type Object struct {
	// ID is the name the backend stores the object under. It is the
	// blob id for every object this store wrote.
	ID string
	// SizeBytes is the object's size as the backend reports it.
	SizeBytes int64
	// ModTime is the backend's own modification time for the object.
	// The orphan sweep ages an object with no row by it.
	ModTime time.Time
}

// Backend stores and retrieves the bytes behind a blob id. It is the
// seam between this package's metadata and the place the bytes live.
// Open installs the disk backend over Config.Dir, and a caller that
// needs another home for its bytes supplies its own implementation.
//
// The interface carries no directory, no rename and no parent handle.
// One id names one immutable object, and a backend owns its own
// namespace layout. A backend that needs a client library lives in its
// own subpackage, so an app that stores on disk never compiles that
// client.
//
// Implementations must be safe for concurrent use and must classify
// their errors, so the store keeps its sentinels. An absent object is
// an error matching ErrNotFound, and a Write against a taken id is one
// matching ErrAlreadyExists. Every other error is a fault the store
// reports as one.
//
// The durability split runs along one line. A backend owns the
// durability of the bytes it accepts. The disk backend fsyncs the file
// and its directory entry before Write returns. An object storage
// backend owns whatever durability its service promises once it
// acknowledges the write. The store owns the order across the seam: it
// writes the metadata row only after Write returns, so a backend that
// returns early weakens the bytes-before-row promise for its own store
// alone.
type Backend interface {
	// Open returns the stored bytes of blobID for reading. An error
	// matching ErrNotFound reports that blobID has no stored bytes.
	Open(ctx context.Context, blobID string) (BlobReader, error)
	// Write stores src as the bytes named blobID and returns the number
	// of bytes it read from src. The bytes are durable under the
	// backend's own guarantees before it returns, because the store
	// writes the metadata row only after. An error matching
	// ErrAlreadyExists reports that blobID already holds bytes, which
	// the call leaves untouched.
	Write(ctx context.Context, blobID string, src io.Reader) (int64, error)
	// Delete removes the stored bytes of blobID. An error matching
	// ErrNotFound reports that blobID has no stored bytes. Any other
	// error is a fault for the caller to log rather than to fail on,
	// because the row that made the bytes reachable is already gone by
	// the time the store deletes.
	Delete(ctx context.Context, blobID string) error
	// List returns every object the backend holds, in no particular
	// order. The store matches each object against the index and ages
	// one with no row by its ModTime.
	List(ctx context.Context) ([]Object, error)
}

// The disk backend sits behind the store by default. It reads the
// store's directory handle and its write seams at call time, because a
// test installs fault injection on those fields after Open returns.
type diskBackend struct {
	store *Store
}

// The disk backend is the Backend Open installs.
var _ Backend = (*diskBackend)(nil)

// Open returns the blob file for blobID. A missing file is classified
// as ErrNotFound, so a vanished blob reads as the same absence an
// unknown id does, and any other failure stays a fault.
func (d *diskBackend) Open(ctx context.Context, blobID string) (BlobReader, error) {
	f, err := d.store.root.Open(blobID)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("open blob %s: %w: %w", blobID, ErrNotFound, err)
		}
		return nil, fmt.Errorf("open blob %s: %w", blobID, err)
	}
	return f, nil
}

// Write copies src into a file created exclusively for blobID through
// writeBlob, which fsyncs the file and then the blob directory, so the
// bytes and the directory entry that names them are on disk before the
// store writes the row. A name already stored is refused before
// anything is written, and the stored bytes are never touched.
func (d *diskBackend) Write(ctx context.Context, blobID string, src io.Reader) (int64, error) {
	size, err := d.store.writeBlob(blobID, src)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			// The exclusive create failed, so the call owns no file and
			// the stored bytes stand.
			return 0, fmt.Errorf("write blob %s: %w: %w", blobID, ErrAlreadyExists, err)
		}
		return 0, err
	}
	return size, nil
}

// Delete removes the blob file. A missing file matches ErrNotFound,
// which is the answer a caller cleaning up a maybe-removed object
// matches.
func (d *diskBackend) Delete(ctx context.Context, blobID string) error {
	err := d.store.root.Remove(blobID)
	if err == nil {
		return nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove blob %s: %w: %w", blobID, ErrNotFound, err)
	}
	return fmt.Errorf("remove blob %s: %w", blobID, err)
}

// List reads the store's directory. A subdirectory is not an object, so
// it is skipped, and so is a file that was removed between the listing
// and its stat, because the next pass will not see it. A file whose
// name is not an id is listed anyway: the backend lists what it holds,
// and the sweep decides what belongs to the store by matching names
// against the index.
func (d *diskBackend) List(ctx context.Context) ([]Object, error) {
	entries, err := os.ReadDir(d.store.dir)
	if err != nil {
		return nil, fmt.Errorf("read blob dir: %w", err)
	}
	objects := make([]Object, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // removed under us, so the next pass will not see it
			}
			return nil, fmt.Errorf("stat %s: %w", entry.Name(), err)
		}
		objects = append(objects, Object{ID: entry.Name(), SizeBytes: info.Size(), ModTime: info.ModTime()})
	}
	return objects, nil
}
