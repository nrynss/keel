package mediastore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/nrynss/keel/mediastore"
)

// seed puts bytes into the backend under a valid id without creating a
// row, which is the state a direct upload leaves behind: the client's
// bytes arrived, and nothing is reachable yet.
func (m *memObjects) seed(blobID string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[blobID] = data
}

// digestOf hashes data the way a client prints the digest it declares.
func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validID(n byte) string { return strings.Repeat(string(n), 32) }

// TestAdoptRecordsVerifiedBytes pins the completion half of a direct
// upload. Bytes the client sent sit in the backend with no row, so
// nothing is reachable, and Adopt records them only after the stored
// object matched the size and the digest the caller expected.
func TestAdoptRecordsVerifiedBytes(t *testing.T) {
	ctx := t.Context()
	objects := newMemObjects()
	rows := newMemRows()
	s, err := mediastore.Open(ctx, mediastore.Config{Index: rows, Backend: objects})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	data := bytes.Repeat([]byte{0x5A}, 4096)
	blobID := validID('a')
	objects.seed(blobID, data)

	if _, err := rows.Get(ctx, blobID); !errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("row before completion = %v, want ErrNotFound", err)
	}
	err = s.Adopt(ctx, blobID, mediastore.Adoption{
		SizeBytes: int64(len(data)),
		SHA256:    digestOf(data),
	}, mediastore.Put{ContentType: "image/png"})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}

	row, err := rows.Get(ctx, blobID)
	if err != nil {
		t.Fatalf("row after completion: %v", err)
	}
	if row.SizeBytes != int64(len(data)) || row.ContentType != "image/png" {
		t.Fatalf("row = %+v, want the verified size and the normalized type", row)
	}

	// The recorded blob serves through the store like any other.
	r, _, err := s.Open(ctx, blobID)
	if err != nil {
		t.Fatalf("open after adopt: %v", err)
	}
	got, readErr := io.ReadAll(r)
	r.Close()
	if readErr != nil {
		t.Fatalf("read after adopt: %v", readErr)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("served %d bytes, want the %d adopted", len(got), len(data))
	}
}

// TestAdoptBeforeUploadIsRefused pins the completion that arrives
// before the bytes. The backend holds nothing, so the call refuses and
// no row lands.
func TestAdoptBeforeUploadIsRefused(t *testing.T) {
	ctx := t.Context()
	objects := newMemObjects()
	rows := newMemRows()
	s, err := mediastore.Open(ctx, mediastore.Config{Index: rows, Backend: objects})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	err = s.Adopt(ctx, validID('b'), mediastore.Adoption{SHA256: digestOf(nil)}, mediastore.Put{ContentType: "image/png"})
	if !errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("adopt before upload = %v, want ErrNotFound", err)
	}
	if _, err := rows.Get(ctx, validID('b')); !errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("row after refused completion = %v, want none", err)
	}
}

// TestAdoptRefusesWrongSizeOrDigest pins verification against the
// stored object. A completion whose size or digest disagrees with the
// bytes refuses, and nothing becomes reachable either way.
func TestAdoptRefusesWrongSizeOrDigest(t *testing.T) {
	ctx := t.Context()
	objects := newMemObjects()
	rows := newMemRows()
	s, err := mediastore.Open(ctx, mediastore.Config{Index: rows, Backend: objects})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	data := bytes.Repeat([]byte{0x3C}, 512)
	blobID := validID('c')
	objects.seed(blobID, data)
	want := mediastore.Adoption{SizeBytes: int64(len(data)), SHA256: digestOf(data)}

	err = s.Adopt(ctx, blobID, mediastore.Adoption{SizeBytes: want.SizeBytes + 1, SHA256: want.SHA256}, mediastore.Put{ContentType: "image/png"})
	if !errors.Is(err, mediastore.ErrSizeMismatch) {
		t.Fatalf("adopt with a wrong size = %v, want ErrSizeMismatch", err)
	}
	err = s.Adopt(ctx, blobID, mediastore.Adoption{SizeBytes: want.SizeBytes, SHA256: strings.Repeat("0", 64)}, mediastore.Put{ContentType: "image/png"})
	if !errors.Is(err, mediastore.ErrDigestMismatch) {
		t.Fatalf("adopt with a wrong digest = %v, want ErrDigestMismatch", err)
	}
	if _, err := rows.Get(ctx, blobID); !errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("row after refused completion = %v, want none", err)
	}
}

// TestAdoptRefusesATakenID pins the id the index already holds. The
// refusal comes before the bytes are read, and the row that stands is
// left exactly as it was.
func TestAdoptRefusesATakenID(t *testing.T) {
	ctx := t.Context()
	objects := newMemObjects()
	rows := newMemRows()
	s, err := mediastore.Open(ctx, mediastore.Config{Index: rows, Backend: objects})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	data := bytes.Repeat([]byte{0x11}, 64)
	blobID := validID('d')
	objects.seed(blobID, data)
	taken, err := s.Persist(ctx, bytes.NewReader([]byte("taken")), mediastore.Put{ContentType: "text/vtt"})
	if err != nil {
		t.Fatalf("persist taken: %v", err)
	}
	takenRow, err := rows.Get(ctx, taken)
	if err != nil {
		t.Fatalf("get taken row: %v", err)
	}

	err = s.Adopt(ctx, taken, mediastore.Adoption{SizeBytes: int64(len(data)), SHA256: digestOf(data)}, mediastore.Put{ContentType: "image/png"})
	if !errors.Is(err, mediastore.ErrAlreadyExists) {
		t.Fatalf("adopt over a taken id = %v, want ErrAlreadyExists", err)
	}
	after, err := rows.Get(ctx, taken)
	if err != nil {
		t.Fatalf("get row after refusal: %v", err)
	}
	if after.CreatedAt != takenRow.CreatedAt || after.SizeBytes != takenRow.SizeBytes {
		t.Fatalf("row changed after a refused adopt: %+v then %+v", takenRow, after)
	}
}

// TestAdoptValidatesItsInput pins the argument refusals. A malformed
// id, a malformed digest and a content type outside the store's set
// refuse before anything is read or written.
func TestAdoptValidatesItsInput(t *testing.T) {
	ctx := t.Context()
	objects := newMemObjects()
	rows := newMemRows()
	s, err := mediastore.Open(ctx, mediastore.Config{Index: rows, Backend: objects})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	want := mediastore.Adoption{SizeBytes: 1, SHA256: digestOf([]byte{1})}

	err = s.Adopt(ctx, "short", want, mediastore.Put{ContentType: "image/png"})
	if !errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("adopt of a malformed id = %v, want ErrNotFound", err)
	}
	err = s.Adopt(ctx, validID('e'), mediastore.Adoption{SizeBytes: 1, SHA256: "zz"}, mediastore.Put{ContentType: "image/png"})
	if !errors.Is(err, mediastore.ErrInvalid) {
		t.Fatalf("adopt with a malformed digest = %v, want ErrInvalid", err)
	}
	err = s.Adopt(ctx, validID('e'), want, mediastore.Put{ContentType: "application/x-unknown"})
	if !errors.Is(err, mediastore.ErrInvalidContentType) {
		t.Fatalf("adopt with an unsupported type = %v, want ErrInvalidContentType", err)
	}
}

// TestAdoptRowFailureKeepsTheBytes pins the failure after verification.
// When the row write fails the bytes stay, because a concurrent
// completion may have won the id, and unreachable bytes age out under
// the orphan sweep.
func TestAdoptRowFailureKeepsTheBytes(t *testing.T) {
	ctx := t.Context()
	objects := newMemObjects()
	data := bytes.Repeat([]byte{0x77}, 128)
	blobID := validID('f')
	objects.seed(blobID, data)
	s, err := mediastore.Open(ctx, mediastore.Config{Index: &refusingRows{memRows: newMemRows()}, Backend: objects})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	err = s.Adopt(ctx, blobID, mediastore.Adoption{SizeBytes: int64(len(data)), SHA256: digestOf(data)}, mediastore.Put{ContentType: "image/png"})
	if err == nil || errors.Is(err, mediastore.ErrAlreadyExists) {
		t.Fatalf("adopt over a refusing index = %v, want a fault", err)
	}
	if got, ok := objects.get(blobID); !ok || !bytes.Equal(got, data) {
		t.Fatal("the verified bytes did not survive a failed row write")
	}
}

// refusingRows is a BlobIndex whose Create always fails, which is how a
// test reaches the row-write failure path no real store hits on
// demand.
type refusingRows struct {
	*memRows
}

func (refusingRows) Create(context.Context, mediastore.Blob) error {
	return errors.New("refusingRows: create refused")
}
