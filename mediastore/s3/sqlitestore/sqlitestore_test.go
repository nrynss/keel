package sqlitestore_test

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nrynss/keel/mediastore"
	"github.com/nrynss/keel/mediastore/s3"
	"github.com/nrynss/keel/mediastore/s3/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// base is the instant the injected clock runs at. It carries a non-zero
// nanosecond part, so a store that lost precision would be caught.
var base = time.Date(2026, 3, 1, 9, 30, 0, 123456789, time.UTC)

// openDB opens the database file at path for a test and closes it at
// cleanup.
func openDB(t *testing.T, path string) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Config{
		Path:   path,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("open sqlite %q: %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() }) // a reopened file closes once per handle, and a close is idempotent
	return db
}

// openStore applies the schema at path and returns the store running on
// the injected clock.
func openStore(t *testing.T, path string) *sqlitestore.Store {
	t.Helper()
	store, err := sqlitestore.Open(t.Context(), sqlitestore.Config{
		DB:  openDB(t, path),
		Now: func() time.Time { return base },
	})
	if err != nil {
		t.Fatalf("open store %q: %v", path, err)
	}
	return store
}

// openFresh opens the database file at path over a new connection with
// no pragmas from any package. It is the independent observer the tests
// query.
func openFresh(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() }) // the handle is discarded here, so a close failure cannot fail the test
	return db
}

// session returns a valid session for a blob id, with the parts a
// two-part plan over 1500 bytes produces.
func session(blobID, uploadID string) sqlitestore.Session {
	return sqlitestore.Session{
		BlobID:      blobID,
		UploadID:    uploadID,
		Owner:       "editor",
		Group:       "takes",
		Visibility:  mediastore.Private,
		ContentType: "video/mp4",
		SizeBytes:   1500,
		SHA256:      strings.Repeat("a", 64),
		PartSize:    1024,
		PartCount:   2,
		CreatedAt:   base,
	}
}

// TestSessionRoundTrip pins what a recorded session keeps. Every field
// a restart or a completion needs reads back unchanged, including the
// visibility the blob will carry.
func TestSessionRoundTrip(t *testing.T) {
	path := t.TempDir() + "/sessions.db"
	store := openStore(t, path)
	want := session(strings.Repeat("a", 32), "upload-1")

	if err := store.Create(t.Context(), want); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.Get(t.Context(), want.BlobID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != want {
		t.Fatalf("session = %+v, want %+v", got, want)
	}

	public := session(strings.Repeat("b", 32), "upload-2")
	public.Visibility = mediastore.Public
	public.CreatedAt = base.Add(time.Second)
	if err := store.Create(t.Context(), public); err != nil {
		t.Fatalf("create public: %v", err)
	}
	got, err = store.Get(t.Context(), public.BlobID)
	if err != nil {
		t.Fatalf("get public: %v", err)
	}
	if got != public {
		t.Fatalf("session = %+v, want %+v", got, public)
	}
}

// TestCreateRefusesDuplicates pins the id the table already holds.
func TestCreateRefusesDuplicates(t *testing.T) {
	store := openStore(t, t.TempDir()+"/sessions.db")
	want := session(strings.Repeat("c", 32), "upload-1")
	if err := store.Create(t.Context(), want); err != nil {
		t.Fatalf("create: %v", err)
	}
	second := want
	second.UploadID = "upload-2"
	if err := store.Create(t.Context(), second); !errors.Is(err, sqlitestore.ErrAlreadyExists) {
		t.Fatalf("create over a taken id = %v, want ErrAlreadyExists", err)
	}
	got, err := store.Get(t.Context(), want.BlobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UploadID != "upload-1" {
		t.Fatalf("upload id moved to %q, want the first session's", got.UploadID)
	}
}

// TestCreateValidatesItsSession pins the refusals for a session this
// package cannot store.
func TestCreateValidatesItsSession(t *testing.T) {
	store := openStore(t, t.TempDir()+"/sessions.db")
	good := session(strings.Repeat("d", 32), "upload-1")
	cases := []struct {
		name string
		bend func(sqlitestore.Session) sqlitestore.Session
	}{
		{"no blob id", func(s sqlitestore.Session) sqlitestore.Session { s.BlobID = ""; return s }},
		{"no upload id", func(s sqlitestore.Session) sqlitestore.Session { s.UploadID = ""; return s }},
		{"no content type", func(s sqlitestore.Session) sqlitestore.Session { s.ContentType = ""; return s }},
		{"negative size", func(s sqlitestore.Session) sqlitestore.Session { s.SizeBytes = -1; return s }},
		{"no digest", func(s sqlitestore.Session) sqlitestore.Session { s.SHA256 = ""; return s }},
		{"empty part", func(s sqlitestore.Session) sqlitestore.Session { s.PartSize = 0; return s }},
		{"no parts", func(s sqlitestore.Session) sqlitestore.Session { s.PartCount = 0; return s }},
		{"past the part bound", func(s sqlitestore.Session) sqlitestore.Session { s.PartCount = s3.MaxParts + 1; return s }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := store.Create(t.Context(), c.bend(good)); !errors.Is(err, sqlitestore.ErrInvalid) {
				t.Fatalf("create = %v, want ErrInvalid", err)
			}
		})
	}
}

// TestZeroCreatedAtIsStampedWithTheStoreClock pins the zero-time rule.
// A session created without a timestamp carries the store clock, so the
// first sweep pass reads it as fresh, and its life runs normally to the
// delete that completes it.
func TestZeroCreatedAtIsStampedWithTheStoreClock(t *testing.T) {
	path := t.TempDir() + "/sessions.db"
	store := openStore(t, path)
	sess := session(strings.Repeat("4", 32), "upload-1")
	sess.CreatedAt = time.Time{}
	if err := store.Create(t.Context(), sess); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.Get(t.Context(), sess.BlobID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(base) {
		t.Fatalf("created at = %s, want the store clock %s", got.CreatedAt, base)
	}

	removed, err := store.Sweep(t.Context(), &stubAborter{}, time.Minute)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if removed != 0 {
		t.Fatalf("first sweep removed %d rows, want the live session untouched", removed)
	}
	if _, err := store.Get(t.Context(), sess.BlobID); err != nil {
		t.Fatalf("get after the sweep: %v", err)
	}
	if err := store.Delete(t.Context(), sess.BlobID); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

// TestGetAndDeleteOfAnUnknownSession pins the absence classifications.
func TestGetAndDeleteOfAnUnknownSession(t *testing.T) {
	store := openStore(t, t.TempDir()+"/sessions.db")
	if _, err := store.Get(t.Context(), strings.Repeat("e", 32)); !errors.Is(err, sqlitestore.ErrNotFound) {
		t.Fatalf("get = %v, want ErrNotFound", err)
	}
	if err := store.Delete(t.Context(), strings.Repeat("e", 32)); !errors.Is(err, sqlitestore.ErrNotFound) {
		t.Fatalf("delete = %v, want ErrNotFound", err)
	}
}

// TestSecondOpenChangesNothing pins the migration ledger. A second open
// over the same file applies no schema a second time.
func TestSecondOpenChangesNothing(t *testing.T) {
	path := t.TempDir() + "/sessions.db"
	store := openStore(t, path)
	if err := store.Create(t.Context(), session(strings.Repeat("f", 32), "upload-1")); err != nil {
		t.Fatalf("create: %v", err)
	}
	again, err := sqlitestore.Open(t.Context(), sqlitestore.Config{DB: openDB(t, path)})
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	if _, err := again.Get(t.Context(), strings.Repeat("f", 32)); err != nil {
		t.Fatalf("get after a second open: %v", err)
	}
}

// stubAborter records every abort it is asked for and fails the ones a
// test marks, which is how the sweep's retry path is driven without a
// bucket.
type stubAborter struct {
	mu      sync.Mutex
	aborted []string
	fail    map[string]bool
}

func (a *stubAborter) AbortUpload(_ context.Context, blobID, _ string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail[blobID] {
		return errors.New("stubAborter: the bucket refused")
	}
	a.aborted = append(a.aborted, blobID)
	return nil
}

// TestSweepAbandonsOldSessionsOnly pins the sweep. A session past the
// bound is aborted in the bucket and its row is deleted, a fresh
// session stays untouched, and the independent observer counts one row
// left.
func TestSweepAbandonsOldSessionsOnly(t *testing.T) {
	path := t.TempDir() + "/sessions.db"
	store := openStore(t, path)
	old := session(strings.Repeat("1", 32), "upload-1")
	fresh := session(strings.Repeat("2", 32), "upload-2")
	old.CreatedAt = base.Add(-time.Hour)
	fresh.CreatedAt = base.Add(-time.Minute)
	for _, sess := range []sqlitestore.Session{old, fresh} {
		if err := store.Create(t.Context(), sess); err != nil {
			t.Fatalf("create %s: %v", sess.BlobID, err)
		}
	}
	aborter := &stubAborter{}

	removed, err := store.Sweep(t.Context(), aborter, 30*time.Minute)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 1 {
		t.Fatalf("sweep removed %d rows, want 1", removed)
	}
	if got := aborter.aborted; len(got) != 1 || got[0] != old.BlobID {
		t.Fatalf("aborted = %v, want the aged session alone", got)
	}
	if _, err := store.Get(t.Context(), old.BlobID); !errors.Is(err, sqlitestore.ErrNotFound) {
		t.Fatalf("get the swept session = %v, want ErrNotFound", err)
	}
	if _, err := store.Get(t.Context(), fresh.BlobID); err != nil {
		t.Fatalf("get the fresh session: %v", err)
	}

	// The independent observer sees the same answer the store gives.
	var held int
	if err := openFresh(t, path).QueryRow(`SELECT count(*) FROM s3_multipart`).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held != 1 {
		t.Fatalf("fresh connection counts %d rows, want 1", held)
	}
}

// TestSweepKeepsARowItsAbortFailed pins the retry path. An upload the
// bucket refuses to discard keeps its row, because removing it would
// orphan parts the service still holds, and the next pass finishes the
// job once the bucket answers.
func TestSweepKeepsARowItsAbortFailed(t *testing.T) {
	path := t.TempDir() + "/sessions.db"
	store := openStore(t, path)
	sess := session(strings.Repeat("3", 32), "upload-1")
	sess.CreatedAt = base.Add(-2 * time.Minute)
	if err := store.Create(t.Context(), sess); err != nil {
		t.Fatalf("create: %v", err)
	}
	aborter := &stubAborter{fail: map[string]bool{sess.BlobID: true}}

	removed, err := store.Sweep(t.Context(), aborter, time.Minute)
	if err != nil {
		t.Fatalf("sweep with a failing aborter = %v, want a logged skip", err)
	}
	if removed != 0 {
		t.Fatalf("sweep removed %d rows, want none", removed)
	}
	if _, err := store.Get(t.Context(), sess.BlobID); err != nil {
		t.Fatalf("get after a failed abort: %v", err)
	}

	aborter = &stubAborter{}
	removed, err = store.Sweep(t.Context(), aborter, time.Minute)
	if err != nil || removed != 1 {
		t.Fatalf("second sweep = %d rows, %v, want 1 with no error", removed, err)
	}
}

// finishingAborter stands in for a completion that wins the race. Its
// abort is the no-op an upload the service no longer holds reads as,
// and it runs the completion path's own row delete before it answers.
type finishingAborter struct {
	store *sqlitestore.Store
}

func (a finishingAborter) AbortUpload(ctx context.Context, blobID, _ string) error {
	return a.store.Delete(ctx, blobID)
}

// TestSweepReadsARemovedRowAsDone pins the race with a completion. A
// row the completion deleted between the sweep's listing and its delete
// is already gone, which is the outcome the sweep wanted, so the pass
// ends clean and counts none of it.
func TestSweepReadsARemovedRowAsDone(t *testing.T) {
	path := t.TempDir() + "/sessions.db"
	store := openStore(t, path)
	sess := session(strings.Repeat("5", 32), "upload-1")
	sess.CreatedAt = base.Add(-time.Hour)
	if err := store.Create(t.Context(), sess); err != nil {
		t.Fatalf("create: %v", err)
	}

	removed, err := store.Sweep(t.Context(), finishingAborter{store: store}, time.Minute)
	if err != nil {
		t.Fatalf("sweep over a removed row = %v, want a clean pass", err)
	}
	if removed != 0 {
		t.Fatalf("sweep counted %d rows, want none of a row the completion took", removed)
	}
	if _, err := store.Get(t.Context(), sess.BlobID); !errors.Is(err, sqlitestore.ErrNotFound) {
		t.Fatalf("get = %v, want ErrNotFound", err)
	}
}

// TestSweepNeedsAnAborter pins the nil refusal.
func TestSweepNeedsAnAborter(t *testing.T) {
	store := openStore(t, t.TempDir()+"/sessions.db")
	if _, err := store.Sweep(t.Context(), nil, time.Minute); !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Fatalf("sweep without an aborter = %v, want ErrInvalid", err)
	}
}
