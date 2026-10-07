package mediastore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// Open returns the stored bytes of id and the row that names them. It
// is the read path for a caller that holds the store as a library
// rather than as a handler. TempCopy is one such caller, and it lands
// the bytes in a file for the ffmpeg based packages.
//
// An unknown, malformed or byte-less id returns an error matching
// ErrNotFound, the same classification the handler answers with a 404.
// The read path checks no visibility and no authorizer. Reading through
// it is an in-process decision, and deciding who may read a private
// blob stays with the caller, the way Config.Authorize decides it for
// the handler. The caller closes the reader. The bytes are immutable
// once stored, so a reader stays good for as long as the caller holds
// it.
func (s *Store) Open(ctx context.Context, blobID string) (BlobReader, Blob, error) {
	b, err := s.find(ctx, blobID)
	if err != nil {
		return nil, Blob{}, err
	}
	r, err := s.backend.Open(ctx, blobID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, Blob{}, notFound(blobID, err)
		}
		return nil, Blob{}, fmt.Errorf("mediastore: open %s: %w", blobID, err)
	}
	return r, b, nil
}

// TempCopy copies the stored bytes of id into a fresh temporary file
// and returns the file's path and a remove function. The file lands in
// the directory os.TempDir names, which matters when that is a small
// tmpfs and the blob is large media. The ffmpeg based packages take a
// file path rather than a reader, so a stored blob reaches them through
// this helper. The file carries no extension, so a consumer that needs
// one renames the path first.
//
// The caller must call remove on every path out of its own code, and
// remove is safe to call more than once. The copy is not fsynced,
// because the file is scratch for the caller's own process and dies
// with it. On a failed call the path is empty, the remove function is
// nil, and no file was created.
func (s *Store) TempCopy(ctx context.Context, blobID string) (string, func(), error) {
	r, _, err := s.Open(ctx, blobID)
	if err != nil {
		return "", nil, err
	}
	defer r.Close()
	f, err := os.CreateTemp("", "mediastore-")
	if err != nil {
		return "", nil, fmt.Errorf("mediastore: temp copy %s: %w", blobID, err)
	}
	path := f.Name()
	remove := sync.OnceFunc(func() {
		_ = os.Remove(path) // best effort, nobody is left to report a failed cleanup to
	})
	if _, err := io.Copy(f, r); err != nil {
		f.Close() // best effort, the file is about to be removed anyway
		remove()
		return "", nil, fmt.Errorf("mediastore: temp copy %s: %w", blobID, err)
	}
	if err := f.Close(); err != nil {
		remove()
		return "", nil, fmt.Errorf("mediastore: temp copy %s: %w", blobID, err)
	}
	return path, remove, nil
}
