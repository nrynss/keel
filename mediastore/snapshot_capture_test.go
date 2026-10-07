package mediastore

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestSnapshotCaptureOfVanishedBytesMatchesNoSentinel pins the
// capture's classification. The capture drives off rows, so bytes that
// vanished between the row and the read are a fault and not an absence.
// The error matched no sentinel before the backend seam, and it matches
// none after it.
func TestSnapshotCaptureOfVanishedBytesMatchesNoSentinel(t *testing.T) {
	s := openTestStore(t)
	a := putBytes(t, s, "aaa", Put{ContentType: "image/png", Owner: "alice"})
	b := putBytes(t, s, "bbb", Put{ContentType: "image/png", Owner: "alice"})
	if err := s.root.Remove(b); err != nil {
		t.Fatalf("remove bytes: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "snap")

	err := s.Snapshot(t.Context(), dir, Selection{IDs: []string{a, b}})

	if err == nil {
		t.Fatal("snapshot over vanished bytes succeeded")
	}
	for _, sentinel := range []error{ErrNotFound, ErrSnapshot, ErrHashMismatch, ErrAlreadyExists} {
		if errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want no match on %v", err, sentinel)
		}
	}
}
