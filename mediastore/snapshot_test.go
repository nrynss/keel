package mediastore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openClockedStore(t *testing.T, now func() time.Time) *Store {
	t.Helper()
	s, err := Open(t.Context(), Config{
		Dir:   filepath.Join(t.TempDir(), "media"),
		Index: newMemIndex(),
		Now:   now,
	})
	if err != nil {
		t.Fatalf("open mediastore: %v", err)
	}
	t.Cleanup(func() { s.root.Close() })
	return s
}

func putBytes(t *testing.T, s *Store, body string, p Put) string {
	t.Helper()
	id, err := s.Persist(t.Context(), strings.NewReader(body), p)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	return id
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	when := time.Date(2024, 3, 1, 12, 0, 0, 123456789, time.UTC)
	var stamps []time.Time
	src := openClockedStore(t, func() time.Time {
		stamps = append(stamps, when.Add(time.Duration(len(stamps))*time.Second))
		return stamps[len(stamps)-1]
	})
	publicID := putBytes(t, src, "png-bytes", Put{
		ContentType: "image/png",
		Owner:       "alice",
		Group:       "book",
		Visibility:  Public,
	})
	privateID := putBytes(t, src, "wav-bytes", Put{
		ContentType: "audio/wav",
		Owner:       "alice",
		Visibility:  Private,
	})
	putBytes(t, src, "other", Put{ContentType: "image/png", Owner: "bob"})

	dir := filepath.Join(t.TempDir(), "snap")
	if err := src.Snapshot(t.Context(), dir, Selection{Owner: "alice"}); err != nil {
		t.Fatalf("snapshot owner: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, manifestFile))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest.Version != ManifestVersion || len(manifest.Blobs) != 2 {
		t.Fatalf("manifest = %+v, want version %d and alice's two blobs", manifest, ManifestVersion)
	}
	if manifest.Blobs[0].ID != publicID || manifest.Blobs[1].ID != privateID {
		t.Fatalf("order = %s then %s, want creation order", manifest.Blobs[0].ID, manifest.Blobs[1].ID)
	}

	later := when.Add(24 * time.Hour)
	dst := openClockedStore(t, func() time.Time { return later })
	if err := dst.Restore(t.Context(), dir); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for _, want := range []struct {
		id   string
		body string
		vis  Visibility
	}{
		{publicID, "png-bytes", Public},
		{privateID, "wav-bytes", Private},
	} {
		got, err := dst.find(t.Context(), want.id)
		if err != nil {
			t.Fatalf("find %s: %v", want.id, err)
		}
		wantAt := stamps[0]
		if want.id == privateID {
			wantAt = stamps[1]
		}
		if got.Owner != "alice" || got.Visibility != want.vis || !got.CreatedAt.Equal(wantAt) {
			t.Fatalf("restored %s = %+v, want alice, visibility %q, created %s", want.id, got, want.vis, wantAt)
		}
		if want.id == publicID && got.Group != "book" {
			t.Fatalf("group = %q, want book", got.Group)
		}
		f, err := dst.root.Open(want.id)
		if err != nil {
			t.Fatalf("open %s: %v", want.id, err)
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(f); err != nil {
			t.Fatalf("read %s: %v", want.id, err)
		}
		f.Close()
		if buf.String() != want.body {
			t.Fatalf("bytes = %q, want %q", buf.String(), want.body)
		}
	}
	listed, err := dst.index.Blobs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("restored %d blobs, want alice's two", len(listed))
	}
}

func TestSnapshotByIDIgnoresOtherBlobs(t *testing.T) {
	s := openTestStore(t)
	keep := putBytes(t, s, "keep", Put{ContentType: "image/png", Owner: "alice"})
	putBytes(t, s, "drop", Put{ContentType: "image/png", Owner: "alice"})
	dir := filepath.Join(t.TempDir(), "snap")
	if err := s.Snapshot(t.Context(), dir, Selection{IDs: []string{keep}, Owner: "bob"}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var manifest Manifest
	raw, err := os.ReadFile(filepath.Join(dir, manifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Blobs) != 1 || manifest.Blobs[0].ID != keep {
		t.Fatalf("manifest blobs = %+v, want only %s", manifest.Blobs, keep)
	}
}

func TestRestoreRefusesHashMismatchAndWritesNothing(t *testing.T) {
	src := openTestStore(t)
	blobID := putBytes(t, src, "png-bytes", Put{ContentType: "image/png", Owner: "alice", Visibility: Public})
	dir := filepath.Join(t.TempDir(), "snap")
	if err := src.Snapshot(t.Context(), dir, Selection{IDs: []string{blobID}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, blobID)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := openTestStore(t)
	err = dst.Restore(t.Context(), dir)
	if !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("restore err = %v, want hash mismatch", err)
	}
	if _, err := dst.find(t.Context(), blobID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("find after refused restore = %v, want not found", err)
	}
}

func TestRestoreRefusesSizeMismatch(t *testing.T) {
	src := openTestStore(t)
	blobID := putBytes(t, src, "png-bytes", Put{ContentType: "image/png"})
	dir := filepath.Join(t.TempDir(), "snap")
	if err := src.Snapshot(t.Context(), dir, Selection{IDs: []string{blobID}}); err != nil {
		t.Fatal(err)
	}
	editManifest(t, dir, func(m *Manifest) { m.Blobs[0].SizeBytes++ })
	dst := openTestStore(t)
	err := dst.Restore(t.Context(), dir)
	if !errors.Is(err, ErrSnapshot) || errors.Is(err, ErrHashMismatch) {
		t.Fatalf("restore err = %v, want invalid snapshot and not a hash mismatch", err)
	}
}

func TestRestoreRefusesUnknownVersion(t *testing.T) {
	src := openTestStore(t)
	blobID := putBytes(t, src, "png-bytes", Put{ContentType: "image/png"})
	dir := filepath.Join(t.TempDir(), "snap")
	if err := src.Snapshot(t.Context(), dir, Selection{IDs: []string{blobID}}); err != nil {
		t.Fatal(err)
	}
	editManifest(t, dir, func(m *Manifest) { m.Version = 2 })
	err := openTestStore(t).Restore(t.Context(), dir)
	if !errors.Is(err, ErrSnapshot) {
		t.Fatalf("restore err = %v, want invalid snapshot", err)
	}
}

func TestRestoreRefusesIDCollisionWithoutTouchingBytes(t *testing.T) {
	src := openTestStore(t)
	blobID := putBytes(t, src, "original", Put{ContentType: "image/png", Owner: "alice"})
	dir := filepath.Join(t.TempDir(), "snap")
	if err := src.Snapshot(t.Context(), dir, Selection{IDs: []string{blobID}}); err != nil {
		t.Fatal(err)
	}
	dst := openTestStore(t)
	if err := dst.PersistWithID(t.Context(), blobID, strings.NewReader("kept"), Put{ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	err := dst.Restore(t.Context(), dir)
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("restore err = %v, want already exists", err)
	}
	f, err := dst.root.Open(blobID)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var buf bytes.Buffer
	buf.ReadFrom(f)
	if buf.String() != "kept" {
		t.Fatalf("bytes = %q, want the pre-existing blob left untouched", buf.String())
	}
}

func TestRestoreRollsBackWhenALaterBlobFails(t *testing.T) {
	src := openTestStore(t)
	first := putBytes(t, src, "one", Put{ContentType: "image/png", Owner: "alice"})
	second := putBytes(t, src, "two", Put{ContentType: "image/png", Owner: "alice"})
	dir := filepath.Join(t.TempDir(), "snap")
	if err := src.Snapshot(t.Context(), dir, Selection{IDs: []string{first, second}}); err != nil {
		t.Fatal(err)
	}
	dst := openTestStore(t)
	dst.index = &failAfter{BlobIndex: dst.index, allow: 1}
	err := dst.Restore(t.Context(), dir)
	if err == nil || errors.Is(err, ErrHashMismatch) {
		t.Fatalf("restore err = %v, want the index refusal", err)
	}
	for _, id := range []string{first, second} {
		if _, findErr := dst.find(t.Context(), id); !errors.Is(findErr, ErrNotFound) {
			t.Fatalf("find %s after rollback = %v, want not found", id, findErr)
		}
		if _, statErr := dst.root.Stat(id); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("stat %s = %v, want the file removed", id, statErr)
		}
	}
}

func TestSnapshotRejectsEmptyAndDuplicateSelection(t *testing.T) {
	s := openTestStore(t)
	dir := filepath.Join(t.TempDir(), "snap")
	if err := s.Snapshot(t.Context(), dir, Selection{}); !errors.Is(err, ErrSnapshot) {
		t.Fatalf("empty selection err = %v, want invalid snapshot", err)
	}
	blobID := putBytes(t, s, "x", Put{ContentType: "image/png"})
	err := s.Snapshot(t.Context(), dir, Selection{IDs: []string{blobID, blobID}})
	if !errors.Is(err, ErrSnapshot) {
		t.Fatalf("duplicate selection err = %v, want invalid snapshot", err)
	}
}

func TestRestoreAcceptsPrivateWordAndEmptySnapshot(t *testing.T) {
	src := openTestStore(t)
	blobID := putBytes(t, src, "x", Put{ContentType: "image/png", Visibility: Private})
	dir := filepath.Join(t.TempDir(), "snap")
	if err := src.Snapshot(t.Context(), dir, Selection{IDs: []string{blobID}}); err != nil {
		t.Fatal(err)
	}
	editManifest(t, dir, func(m *Manifest) { m.Blobs[0].Visibility = "private" })
	dst := openTestStore(t)
	if err := dst.Restore(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	got, err := dst.find(t.Context(), blobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Visibility != Private {
		t.Fatalf("visibility = %q, want private", got.Visibility)
	}

	empty := filepath.Join(t.TempDir(), "empty")
	if err := src.Snapshot(t.Context(), empty, Selection{Owner: "nobody"}); err != nil {
		t.Fatal(err)
	}
	if err := openTestStore(t).Restore(t.Context(), empty); err != nil {
		t.Fatalf("empty restore: %v", err)
	}
}

func TestSnapshotCancelledContext(t *testing.T) {
	s := openTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := s.Snapshot(ctx, filepath.Join(t.TempDir(), "snap"), Selection{Owner: "alice"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}
}

// failAfter refuses Create once allow successes have happened. The other
// index methods pass through, so preflight still sees a missing id.
type failAfter struct {
	BlobIndex
	allow int
	n     int
}

func (f *failAfter) Create(ctx context.Context, b Blob) error {
	if f.n >= f.allow {
		return errors.New("index refused")
	}
	if err := f.BlobIndex.Create(ctx, b); err != nil {
		return err
	}
	f.n++
	return nil
}

func editManifest(t *testing.T, dir string, edit func(*Manifest)) {
	t.Helper()
	path := filepath.Join(dir, manifestFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	edit(&manifest)
	raw, err = json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
