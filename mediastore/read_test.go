package mediastore

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
)

// TestOpenReturnsBytesAndRow pins the read path. One call returns the
// stored bytes whole and the row that names them. The reader seeks,
// because the handler's Range answers and every copy ride on Seek.
func TestOpenReturnsBytesAndRow(t *testing.T) {
	s := openTestStore(t)
	data := blob(4096)
	blobID, err := s.Persist(t.Context(), bytes.NewReader(data), Put{
		ContentType: "audio/mpeg",
		Owner:       "alice",
		Group:       "g1",
		Visibility:  Public,
	})
	if err != nil {
		t.Fatalf("persist: %v", err)
	}

	r, got, err := s.Open(t.Context(), blobID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()

	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(body, data) {
		t.Fatalf("read %d bytes, want the %d stored", len(body), len(data))
	}
	if got.ID != blobID || got.Owner != "alice" || got.Group != "g1" || got.ContentType != "audio/mpeg" {
		t.Fatalf("row = %+v, want the persisted metadata", got)
	}
	if got.Visibility != Public || got.SizeBytes != int64(len(data)) {
		t.Fatalf("row = %+v, want public visibility and %d bytes", got, len(data))
	}

	if _, err := r.Seek(2, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	tail, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read after seek: %v", err)
	}
	if !bytes.Equal(tail, data[2:]) {
		t.Fatalf("read after seek = %d bytes, want the %d from offset 2", len(tail), len(data)-2)
	}
}

// TestOpenClassifiesAbsentBlobsAsNotFound: an unknown id, a malformed
// one, and a row whose bytes vanished all match ErrNotFound, the same
// classification the handler answers with a 404. No reader comes back
// alongside the error.
func TestOpenClassifiesAbsentBlobsAsNotFound(t *testing.T) {
	s := openTestStore(t)
	const vanished = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := s.index.Create(t.Context(), Blob{ID: vanished, ContentType: "image/png", SizeBytes: 3}); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	cases := []string{
		"ffffffffffffffffffffffffffffffff", // well formed, no row
		"short",                            // malformed
		vanished,                           // row without bytes
	}
	for _, blobID := range cases {
		t.Run(blobID, func(t *testing.T) {
			r, _, err := s.Open(t.Context(), blobID)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
			if r != nil {
				r.Close()
				t.Fatal("open returned a reader alongside an error")
			}
		})
	}
}

// TestTempCopyServesBytesAndRemoveDeletesTheFile pins the helper's two
// promises: the copy carries the stored bytes whole, and remove takes
// the file away, including on a second call.
func TestTempCopyServesBytesAndRemoveDeletesTheFile(t *testing.T) {
	s := openTestStore(t)
	data := blob(2048)
	blobID := persistPublic(t, s, "application/pdf", data)

	path, remove, err := s.TempCopy(t.Context(), blobID)
	if err != nil {
		t.Fatalf("temp copy: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp copy: %v", err)
	}
	if !bytes.Equal(raw, data) {
		t.Fatalf("temp copy = %d bytes, want the %d stored", len(raw), len(data))
	}

	remove()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat after remove = %v, want the file gone", err)
	}
	remove() // a second call must be a no-op, not a panic or an error
}

// TestTempCopyReadsAPrivateBlob: the read path is an in-process
// decision, so a private blob copies with no authorizer configured. The
// authorizer governs the handler, not the library caller.
func TestTempCopyReadsAPrivateBlob(t *testing.T) {
	s := openTestStore(t)
	data := blob(64)
	blobID, err := s.Persist(t.Context(), bytes.NewReader(data), Put{ContentType: "image/png"})
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	path, remove, err := s.TempCopy(t.Context(), blobID)
	if err != nil {
		t.Fatalf("temp copy: %v", err)
	}
	defer remove()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp copy: %v", err)
	}
	if !bytes.Equal(raw, data) {
		t.Fatalf("temp copy = %d bytes, want the %d stored", len(raw), len(data))
	}
}

// TestTempCopyFailureLeavesNoHandle: a failed call returns no path and
// no remove function, so a caller has nothing to clean up.
func TestTempCopyFailureLeavesNoHandle(t *testing.T) {
	s := openTestStore(t)
	path, remove, err := s.TempCopy(t.Context(), "ffffffffffffffffffffffffffffffff")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if path != "" || remove != nil {
		t.Fatalf("path = %q, remove = %v, want neither on a failed call", path, remove != nil)
	}
}
