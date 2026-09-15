package sqlitestore_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nrynss/keel/mediastore"
	"github.com/nrynss/keel/mediastore/sqlitestore"
	"github.com/nrynss/keel/sqlite"
)

// base is the instant the tests stamp rows with. It carries a non-zero
// nanosecond part so a database that stored whole seconds would be
// caught.
var base = time.Date(2024, 3, 1, 12, 0, 0, 123456789, time.UTC)

// openDB opens a database at path. Each test gets its own file unless it
// deliberately reopens the same one.
func openDB(t *testing.T, path string) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Config{
		Path:   path,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// openIndex returns the media index over a fresh database file.
// openIndexWithDB returns the media index and the database handle it
// runs on, so a test can read the migration ledger the runner keeps.
func openIndexWithDB(t *testing.T, path string) (*sqlitestore.Store, *sqlite.DB) {
	t.Helper()
	db := openDB(t, path)
	idx, err := sqlitestore.Open(t.Context(), sqlitestore.Config{DB: db})
	if err != nil {
		t.Fatalf("open media index: %v", err)
	}
	return idx, db
}

// openIndex returns the media index over a fresh database file.
func openIndex(t *testing.T) *sqlitestore.Store {
	t.Helper()
	idx, _ := openIndexWithDB(t, filepath.Join(t.TempDir(), "media.db"))
	return idx
}

// openIndexAt returns the media index over the database file at path, so
// a test can come back to a database an earlier index already shaped.
func openIndexAt(t *testing.T, path string) *sqlitestore.Store {
	t.Helper()
	idx, _ := openIndexWithDB(t, path)
	return idx
}

// hourTicker returns a clock that reports one hour later on every read,
// so a sequence of writes gets distinct creation times.
func hourTicker() func() time.Time {
	at := base
	return func() time.Time {
		now := at
		at = at.Add(time.Hour)
		return now
	}
}

// ledgerFilenames lists the migration files one namespace recorded. The
// sqlite package names a namespace's ledger after it, and the ledger is
// what proves the runner owns the schema.
func ledgerFilenames(t *testing.T, db *sqlite.DB, namespace string) []string {
	t.Helper()
	rows, err := db.Reader().QueryContext(t.Context(),
		`SELECT filename FROM "`+namespace+`_schema_migrations" ORDER BY filename`)
	if err != nil {
		t.Fatalf("read migration ledger: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan ledger row: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("drain ledger rows: %v", err)
	}
	return names
}

// TestCreateGetRoundTrip pins that every field survives the database
// unchanged, because a field that silently defaults is how an access
// rule or an age goes wrong.
func TestCreateGetRoundTrip(t *testing.T) {
	idx := openIndex(t)
	want := mediastore.Blob{
		ID:          "0123456789abcdef0123456789abcdef",
		Owner:       "creator-7",
		Group:       "clip-42",
		ContentType: "audio/ogg",
		SizeBytes:   4096,
		Visibility:  mediastore.Public,
		CreatedAt:   base,
	}
	if err := idx.Create(t.Context(), want); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := idx.Get(t.Context(), want.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.ID != want.ID || got.Owner != want.Owner || got.Group != want.Group {
		t.Fatalf("identity fields = %+v, want %+v", got, want)
	}
	if got.ContentType != want.ContentType || got.SizeBytes != want.SizeBytes {
		t.Fatalf("content fields = %+v, want %+v", got, want)
	}
	if got.Visibility != mediastore.Public {
		t.Fatalf("visibility = %q, want public", string(got.Visibility))
	}
	if got.CreatedAt.UnixNano() != want.CreatedAt.UnixNano() {
		t.Fatalf("created at = %v, want %v", got.CreatedAt, want.CreatedAt)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Fatalf("created at location = %v, want UTC", got.CreatedAt.Location())
	}
}

// TestVisibilityDefaultsToPrivateOnStorage: a row stored without a
// visibility reads back private, so a caller that forgot the field never
// gets a world readable row.
func TestVisibilityDefaultsToPrivateOnStorage(t *testing.T) {
	idx := openIndex(t)
	row := mediastore.Blob{
		ID:          "abcdef0123456789abcdef0123456789",
		ContentType: "image/png",
		CreatedAt:   base,
	}
	if err := idx.Create(t.Context(), row); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := idx.Get(t.Context(), row.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.Visibility != mediastore.Private {
		t.Fatalf("visibility = %q, want private", string(got.Visibility))
	}
}

// TestCreateDuplicateReportsAlreadyExists: a taken id is refused through
// the sentinel a caller classifies, and the first row is untouched.
func TestCreateDuplicateReportsAlreadyExists(t *testing.T) {
	idx := openIndex(t)
	first := mediastore.Blob{
		ID:          "11111111111111111111111111111111",
		ContentType: "image/png",
		SizeBytes:   10,
		Owner:       "first",
		CreatedAt:   base,
	}
	if err := idx.Create(t.Context(), first); err != nil {
		t.Fatalf("create: %v", err)
	}

	second := first
	second.Owner = "second"
	second.SizeBytes = 999
	err := idx.Create(t.Context(), second)
	if !errors.Is(err, mediastore.ErrAlreadyExists) {
		t.Fatalf("err = %v, want ErrAlreadyExists", err)
	}

	got, err := idx.Get(t.Context(), first.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Owner != "first" || got.SizeBytes != 10 {
		t.Fatalf("row = %+v, want the first write left alone", got)
	}
}

// TestGetMissingReportsNotFound: a missing row is the sentinel a
// mediastore caller classifies, not a bare driver error.
func TestGetMissingReportsNotFound(t *testing.T) {
	idx := openIndex(t)

	_, err := idx.Get(t.Context(), "22222222222222222222222222222222")

	if !errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestDeleteRemovesTheRowAndMissingReportsNotFound: the delete is
// idempotent by classification, so a caller can tell "gone" from
// "broken".
func TestDeleteRemovesTheRowAndMissingReportsNotFound(t *testing.T) {
	idx := openIndex(t)
	row := mediastore.Blob{
		ID:          "33333333333333333333333333333333",
		ContentType: "video/mp4",
		CreatedAt:   base,
	}
	if err := idx.Create(t.Context(), row); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := idx.Delete(t.Context(), row.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := idx.Get(t.Context(), row.ID); !errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("get after delete = %v, want ErrNotFound", err)
	}
	if err := idx.Delete(t.Context(), row.ID); !errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
}

// TestDeleteGroupRemovesEveryRowInTheGroup: the group is the eviction
// unit, so one call must leave none of it behind.
func TestDeleteGroupRemovesEveryRowInTheGroup(t *testing.T) {
	idx := openIndex(t)
	for i, id := range []string{
		"44444444444444444444444444444444",
		"55555555555555555555555555555555",
	} {
		row := mediastore.Blob{
			ID:          id,
			Group:       "clip-1",
			ContentType: "audio/mpeg",
			CreatedAt:   base.Add(time.Duration(i) * time.Second),
		}
		if err := idx.Create(t.Context(), row); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	keeper := mediastore.Blob{
		ID:          "66666666666666666666666666666666",
		Group:       "clip-2",
		ContentType: "audio/mpeg",
		CreatedAt:   base,
	}
	if err := idx.Create(t.Context(), keeper); err != nil {
		t.Fatalf("create keeper: %v", err)
	}

	if err := idx.DeleteGroup(t.Context(), "clip-1"); err != nil {
		t.Fatalf("delete group: %v", err)
	}

	groups, err := idx.Groups(t.Context())
	if err != nil {
		t.Fatalf("groups: %v", err)
	}
	if len(groups) != 1 || groups[0].ID != "clip-2" || len(groups[0].Blobs) != 1 {
		t.Fatalf("groups = %+v, want only clip-2", groups)
	}
	if err := idx.DeleteGroup(t.Context(), "clip-1"); !errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("second delete group = %v, want ErrNotFound", err)
	}
	if err := idx.DeleteGroup(t.Context(), ""); !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Fatalf("empty group = %v, want ErrInvalid", err)
	}
}

// TestGroupsOrdersByIDAndCarriesTheEarliestCreation: the budget evicts
// oldest first off this listing, so the order and the group age it
// reports both have to be right.
func TestGroupsOrdersByIDAndCarriesTheEarliestCreation(t *testing.T) {
	idx := openIndex(t)
	rows := []mediastore.Blob{
		{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Group: "zebra", ContentType: "image/png", CreatedAt: base.Add(2 * time.Hour)},
		{ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Group: "zebra", ContentType: "image/png", CreatedAt: base},
		{ID: "cccccccccccccccccccccccccccccccc", Group: "alpha", ContentType: "image/png", CreatedAt: base.Add(time.Hour)},
		{ID: "dddddddddddddddddddddddddddddddd", ContentType: "image/png", CreatedAt: base},
	}
	for _, row := range rows {
		if err := idx.Create(t.Context(), row); err != nil {
			t.Fatalf("create %s: %v", row.ID, err)
		}
	}

	groups, err := idx.Groups(t.Context())
	if err != nil {
		t.Fatalf("groups: %v", err)
	}

	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2 (the unplaced row is not a group)", len(groups))
	}
	if groups[0].ID != "alpha" || groups[1].ID != "zebra" {
		t.Fatalf("group order = %s, %s, want alpha then zebra", groups[0].ID, groups[1].ID)
	}
	if groups[1].CreatedAt.UnixNano() != base.UnixNano() {
		t.Fatalf("zebra created at = %v, want the earliest blob %v", groups[1].CreatedAt, base)
	}
	if len(groups[1].Blobs) != 2 {
		t.Fatalf("zebra blobs = %d, want 2", len(groups[1].Blobs))
	}
	if groups[1].Blobs[0].ID != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("zebra blobs = %+v, want earliest first", groups[1].Blobs)
	}
}

// TestCreateRejectsUnstorableRows: this package refuses a row it cannot
// describe rather than storing a broken one.
func TestCreateRejectsUnstorableRows(t *testing.T) {
	idx := openIndex(t)
	cases := []struct {
		name string
		row  mediastore.Blob
	}{
		{"empty id", mediastore.Blob{ContentType: "image/png"}},
		{"empty content type", mediastore.Blob{ID: "77777777777777777777777777777777"}},
		{"negative size", mediastore.Blob{ID: "88888888888888888888888888888888", ContentType: "image/png", SizeBytes: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := idx.Create(t.Context(), tc.row); !errors.Is(err, sqlitestore.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

// TestOpenRejectsANilDatabase: the index has nowhere to put anything, so
// Open says so instead of panicking later.
func TestOpenRejectsANilDatabase(t *testing.T) {
	_, err := sqlitestore.Open(t.Context(), sqlitestore.Config{})

	if !errors.Is(err, sqlitestore.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

// TestOpenRunsTheMigrationsOnce: a process that restarts must find its
// rows and must not re-run or re-record its schema, which is what makes
// the runner the owner of the schema.
func TestOpenRunsTheMigrationsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "media.db")
	first := openDB(t, path)
	idx, err := sqlitestore.Open(t.Context(), sqlitestore.Config{DB: first})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	row := mediastore.Blob{
		ID:          "99999999999999999999999999999999",
		Group:       "clip-9",
		ContentType: "application/pdf",
		SizeBytes:   2048,
		Visibility:  mediastore.Public,
		CreatedAt:   base,
	}
	if err := idx.Create(t.Context(), row); err != nil {
		t.Fatalf("create: %v", err)
	}
	before := ledgerFilenames(t, first, "mediastore")
	if len(before) != 2 {
		t.Fatalf("migrations recorded = %v, want the two schema files", before)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close first handle: %v", err)
	}
	second, secondDB := openIndexWithDB(t, path)

	after := ledgerFilenames(t, secondDB, "mediastore")
	if len(after) != len(before) {
		t.Fatalf("migrations recorded = %v, want %v after a second open", after, before)
	}
	got, err := second.Get(t.Context(), row.ID)
	if err != nil {
		t.Fatalf("get after restart: %v", err)
	}
	if got.SizeBytes != row.SizeBytes || got.Visibility != mediastore.Public {
		t.Fatalf("row after restart = %+v, want %+v", got, row)
	}
}

// TestStoreOverTheSQLiteIndex wires the two packages together: the store
// persists through the real index, serves what it stored, and the
// retention sweep evicts a group through the real group query.
func TestStoreOverTheSQLiteIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "media.db")
	dir := filepath.Join(t.TempDir(), "blobs")
	var index mediastore.BlobIndex = openIndexAt(t, path)
	s, err := mediastore.Open(t.Context(), mediastore.Config{
		Dir:   dir,
		Index: index,
		Now:   hourTicker(),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	served, err := s.Persist(t.Context(), bytes.NewReader([]byte("hello keel")), mediastore.Put{
		ContentType: "text/vtt",
		Visibility:  mediastore.Public,
	})
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/media/"+served, nil)
	req.SetPathValue("id", served)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || rr.Body.String() != "hello keel" {
		t.Fatalf("served %d %q, want 200 with the stored bytes", rr.Code, rr.Body.String())
	}

	evictable, err := s.Persist(t.Context(), bytes.NewReader(bytes.Repeat([]byte("x"), 4096)), mediastore.Put{
		ContentType: "audio/mpeg",
		Group:       "clip-old",
		Visibility:  mediastore.Public,
	})
	if err != nil {
		t.Fatalf("persist evictable: %v", err)
	}
	keeper, err := s.Persist(t.Context(), bytes.NewReader([]byte("y")), mediastore.Put{
		ContentType: "audio/mpeg",
		Group:       "clip-new",
		Visibility:  mediastore.Public,
	})
	if err != nil {
		t.Fatalf("persist keeper: %v", err)
	}
	w, err := s.NewSweeper(mediastore.RetentionConfig{
		Now:         func() time.Time { return base.Add(2 * time.Hour) },
		MaxBytes:    2048,
		MinGroups:   1,
		MinGroupAge: time.Minute,
	})
	if err != nil {
		t.Fatalf("new sweeper: %v", err)
	}

	result, err := w.Sweep(t.Context())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if result.GroupsEvicted != 1 {
		t.Fatalf("evicted = %d, want 1", result.GroupsEvicted)
	}
	if _, err := os.Stat(filepath.Join(dir, evictable)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("evicted blob file still present: %v", err)
	}
	if _, err := index.Get(t.Context(), evictable); !errors.Is(err, mediastore.ErrNotFound) {
		t.Fatalf("evicted row still present: %v", err)
	}
	if _, err := index.Get(t.Context(), keeper); err != nil {
		t.Fatalf("kept row went missing: %v", err)
	}
}

// TestStoreRefusedPersistWithIDKeepsTheStoredBlob: a failed insert must
// never destroy a stored blob. A second persist for an id the store
// already holds is refused, and the file the first persist stored stays
// on disk with the same bytes and is still served.
func TestStoreRefusedPersistWithIDKeepsTheStoredBlob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "media.db")
	dir := filepath.Join(t.TempDir(), "blobs")
	var index mediastore.BlobIndex = openIndexAt(t, path)
	s, err := mediastore.Open(t.Context(), mediastore.Config{Dir: dir, Index: index})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	const blobID = "0123456789abcdef0123456789abcdef"
	data := bytes.Repeat([]byte("stored"), 20)
	if err := s.PersistWithID(t.Context(), blobID, bytes.NewReader(data), mediastore.Put{
		ContentType: "image/png",
		Visibility:  mediastore.Public,
	}); err != nil {
		t.Fatalf("first persist: %v", err)
	}
	want := sha256.Sum256(data)

	err = s.PersistWithID(t.Context(), blobID, bytes.NewReader(bytes.Repeat([]byte("x"), 5)), mediastore.Put{
		ContentType: "image/png",
		Visibility:  mediastore.Public,
	})
	if !errors.Is(err, mediastore.ErrAlreadyExists) {
		t.Fatalf("retry err = %v, want ErrAlreadyExists", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, blobID))
	if err != nil {
		t.Fatalf("stored blob file gone after a refused retry: %v", err)
	}
	if sha256.Sum256(got) != want {
		t.Fatal("stored blob bytes changed after a refused retry")
	}

	row, err := index.Get(t.Context(), blobID)
	if err != nil || row.SizeBytes != int64(len(data)) {
		t.Fatalf("row = %+v err = %v, want SizeBytes %d", row, err, len(data))
	}

	req := httptest.NewRequest(http.MethodGet, "/media/"+blobID, nil)
	req.SetPathValue("id", blobID)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), data) {
		t.Fatalf("served %d after a refused retry, want 200 with the stored bytes", rr.Code)
	}
}
