package mediastore_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/nrynss/keel/mediastore"
)

// This file exercises the supplied-backend seam from outside the
// package, the way an object storage implementation in its own
// subpackage will. A Backend reaches the store through Config, the
// bytes never touch a disk, and the store's own operations keep their
// classifications.

var (
	_ mediastore.BlobIndex = (*memRows)(nil)
	_ mediastore.Backend   = (*memObjects)(nil)
)

// memRows is an in-memory BlobIndex for the external seam test. A
// missing row wraps mediastore.ErrNotFound and a taken id wraps
// mediastore.ErrAlreadyExists, which is the contract an implementation
// outside the package follows.
type memRows struct {
	mu   sync.Mutex
	rows map[string]mediastore.Blob
}

func newMemRows() *memRows {
	return &memRows{rows: map[string]mediastore.Blob{}}
}

func (m *memRows) Create(_ context.Context, b mediastore.Blob) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[b.ID]; ok {
		return fmt.Errorf("memrows: id taken: %w", mediastore.ErrAlreadyExists)
	}
	m.rows[b.ID] = b
	return nil
}

func (m *memRows) Get(_ context.Context, blobID string) (mediastore.Blob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.rows[blobID]
	if !ok {
		return mediastore.Blob{}, fmt.Errorf("memrows: no such row: %w", mediastore.ErrNotFound)
	}
	return b, nil
}

func (m *memRows) Delete(_ context.Context, blobID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[blobID]; !ok {
		return fmt.Errorf("memrows: no such row: %w", mediastore.ErrNotFound)
	}
	delete(m.rows, blobID)
	return nil
}

func (m *memRows) DeleteGroup(_ context.Context, group string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	found := false
	for blobID, b := range m.rows {
		if b.Group == group {
			delete(m.rows, blobID)
			found = true
		}
	}
	if !found {
		return fmt.Errorf("memrows: no such group: %w", mediastore.ErrNotFound)
	}
	return nil
}

func (m *memRows) Groups(_ context.Context) ([]mediastore.Group, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	byGroup := map[string][]mediastore.Blob{}
	for _, b := range m.rows {
		if b.Group != "" {
			byGroup[b.Group] = append(byGroup[b.Group], b)
		}
	}
	out := make([]mediastore.Group, 0, len(byGroup))
	for groupID, blobs := range byGroup {
		sort.Slice(blobs, func(i, j int) bool { return blobs[i].CreatedAt.Before(blobs[j].CreatedAt) })
		out = append(out, mediastore.Group{ID: groupID, CreatedAt: blobs[0].CreatedAt, Blobs: blobs})
	}
	return out, nil
}

func (m *memRows) Blobs(_ context.Context) ([]mediastore.Blob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]mediastore.Blob, 0, len(m.rows))
	for _, b := range m.rows {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// memObjects is an in-memory Backend for the external seam test. It
// classifies an absent object as ErrNotFound and a taken id as
// ErrAlreadyExists, the way the interface contract requires.
type memObjects struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newMemObjects() *memObjects {
	return &memObjects{objects: map[string][]byte{}}
}

// get reads one stored object back, so the test can see what the store
// wrote without going through the store again.
func (m *memObjects) get(blobID string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[blobID]
	return b, ok
}

func (m *memObjects) Open(_ context.Context, blobID string) (mediastore.BlobReader, error) {
	m.mu.Lock()
	b, ok := m.objects[blobID]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("memobjects: no such object: %w", mediastore.ErrNotFound)
	}
	return &sliceReader{Reader: bytes.NewReader(b)}, nil
}

func (m *memObjects) Write(_ context.Context, blobID string, src io.Reader) (int64, error) {
	data, err := io.ReadAll(src)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objects[blobID]; ok {
		return 0, fmt.Errorf("memobjects: object %s exists: %w", blobID, mediastore.ErrAlreadyExists)
	}
	m.objects[blobID] = data
	return int64(len(data)), nil
}

func (m *memObjects) Delete(_ context.Context, blobID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objects[blobID]; !ok {
		return fmt.Errorf("memobjects: no such object: %w", mediastore.ErrNotFound)
	}
	delete(m.objects, blobID)
	return nil
}

func (m *memObjects) List(_ context.Context) ([]mediastore.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]mediastore.Object, 0, len(m.objects))
	for blobID, data := range m.objects {
		out = append(out, mediastore.Object{ID: blobID, SizeBytes: int64(len(data)), ModTime: time.Now().UTC()})
	}
	return out, nil
}

// sliceReader is a bytes.Reader with a Close, because BlobReader asks
// for one and bytes.Reader has none.
type sliceReader struct {
	*bytes.Reader
}

func (*sliceReader) Close() error { return nil }

// TestConfigBackendCarriesTheBytes pins the supplied-backend seam. A
// Backend reaches the store through Config, no directory is required
// beside it, and the store's operations keep their classifications over
// a foreign implementation.
func TestConfigBackendCarriesTheBytes(t *testing.T) {
	ctx := t.Context()
	objects := newMemObjects()
	s, err := mediastore.Open(ctx, mediastore.Config{
		Index:   newMemRows(),
		Backend: objects,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	data := bytes.Repeat([]byte{0xA5}, 4096)
	blobID, err := s.Persist(ctx, bytes.NewReader(data), mediastore.Put{
		ContentType: "image/png",
		Visibility:  mediastore.Public,
	})
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	stored, ok := objects.get(blobID)
	if !ok {
		t.Fatal("the backend holds nothing under the returned id")
	}
	if !bytes.Equal(stored, data) {
		t.Fatalf("the backend holds %d bytes, want the %d persisted", len(stored), len(data))
	}

	r, row, err := s.Open(ctx, blobID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("read %d bytes, want the %d stored", len(got), len(data))
	}
	if row.ContentType != "image/png" || row.SizeBytes != int64(len(data)) {
		t.Fatalf("row = %+v, want the persisted metadata", row)
	}

	path, remove, err := s.TempCopy(ctx, blobID)
	if err != nil {
		t.Fatalf("temp copy: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp copy: %v", err)
	}
	remove()
	if !bytes.Equal(raw, data) {
		t.Fatal("temp copy does not carry the stored bytes")
	}

	if err := s.Delete(ctx, blobID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := objects.get(blobID); ok {
		t.Fatal("the object survived the delete")
	}
	if _, _, err := s.Open(ctx, blobID); !errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("open after delete = %v, want ErrNotFound", err)
	}
}
