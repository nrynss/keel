package mediastore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nrynss/keel/id"
)

// The sentinels memIndex raises. A missing row wraps ErrNotFound and a
// taken id wraps ErrAlreadyExists, which is what the BlobIndex contract
// requires. Each keeps its own value too, so a test can see the index's
// own error survive the store's classification. errIndexDown stands in
// for a database that went away, which is how a test reaches the
// server-fault branches.
var (
	errMemNotFound      = fmt.Errorf("memindex: no such row: %w", ErrNotFound)
	errMemAlreadyExists = fmt.Errorf("memindex: id taken: %w", ErrAlreadyExists)
	errIndexDown        = errors.New("memindex: closed")
)

// memIndex is an in-memory BlobIndex. The store's own tests need an
// index they can drive and shut down at will, and the SQLite index has
// its own tests against a real database.
type memIndex struct {
	mu   sync.Mutex
	rows map[string]Blob
	down bool
}

// newMemIndex returns an empty index.
func newMemIndex() *memIndex {
	return &memIndex{rows: map[string]Blob{}}
}

// shut makes every later call fail, which is how a test reaches the
// server-fault branches without a real database to close.
func (m *memIndex) shut() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.down = true
}

func (m *memIndex) Create(ctx context.Context, b Blob) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return errIndexDown
	}
	if _, ok := m.rows[b.ID]; ok {
		return errMemAlreadyExists
	}
	m.rows[b.ID] = b
	return nil
}

func (m *memIndex) Get(ctx context.Context, blobID string) (Blob, error) {
	if err := ctx.Err(); err != nil {
		return Blob{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return Blob{}, errIndexDown
	}
	b, ok := m.rows[blobID]
	if !ok {
		return Blob{}, errMemNotFound
	}
	return b, nil
}

func (m *memIndex) Delete(ctx context.Context, blobID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return errIndexDown
	}
	if _, ok := m.rows[blobID]; !ok {
		return errMemNotFound
	}
	delete(m.rows, blobID)
	return nil
}

func (m *memIndex) DeleteGroup(ctx context.Context, group string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return errIndexDown
	}
	found := false
	for blobID, b := range m.rows {
		if b.Group == group {
			delete(m.rows, blobID)
			found = true
		}
	}
	if !found {
		return errMemNotFound
	}
	return nil
}

func (m *memIndex) Groups(ctx context.Context) ([]Group, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return nil, errIndexDown
	}
	byGroup := map[string][]Blob{}
	for _, b := range m.rows {
		if b.Group != "" {
			byGroup[b.Group] = append(byGroup[b.Group], b)
		}
	}
	groups := make([]Group, 0, len(byGroup))
	for groupID, blobs := range byGroup {
		sort.Slice(blobs, func(i, j int) bool { return blobs[i].CreatedAt.Before(blobs[j].CreatedAt) })
		groups = append(groups, Group{ID: groupID, CreatedAt: blobs[0].CreatedAt, Blobs: blobs})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })
	return groups, nil
}

// openTestStore builds a store over a throwaway directory and an
// in-memory index.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), Config{
		Dir:   filepath.Join(t.TempDir(), "media"),
		Index: newMemIndex(),
	})
	if err != nil {
		t.Fatalf("open mediastore: %v", err)
	}
	t.Cleanup(func() { s.root.Close() })
	return s
}

// serve runs one request the way the mux does, with the path value
// already resolved.
func serve(t *testing.T, s *Store, method, blobID string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/media/"+blobID, nil)
	req.SetPathValue("id", blobID)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	return rr
}

// blob returns deterministic bytes of the given length, patterned so a
// byte-slice assertion cannot pass by accident.
func blob(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// persistPublic writes src as a public blob, which is the visibility the
// served-back tests need because no authorizer is configured.
func persistPublic(t *testing.T, s *Store, contentType string, data []byte) string {
	t.Helper()
	blobID, err := s.Persist(t.Context(), bytes.NewReader(data), Put{
		ContentType: contentType,
		Visibility:  Public,
	})
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	return blobID
}

// openTestStoreWith builds a store like openTestStore, after the caller
// adjusts the Config. It is how a test reaches the authorizer branch.
func openTestStoreWith(t *testing.T, mutate func(*Config)) *Store {
	t.Helper()
	cfg := Config{
		Dir:   filepath.Join(t.TempDir(), "media"),
		Index: newMemIndex(),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("open mediastore: %v", err)
	}
	t.Cleanup(func() { s.root.Close() })
	return s
}

// TestPrivateIsTheZeroValueAndTheDefault pins the deny by default rule.
// A caller that forgets the field must get a blob only its authorizer
// may read, so the zero value of Visibility is Private and an unset Put
// stores private.
func TestPrivateIsTheZeroValueAndTheDefault(t *testing.T) {
	if Private != "" {
		t.Fatalf("Private = %q, want the zero value so an unset field stays private", string(Private))
	}
	var unset Visibility
	if unset != Private {
		t.Fatalf("zero Visibility = %q, want Private", string(unset))
	}
	if Public == Private {
		t.Fatal("Public must differ from Private")
	}
	s := openTestStoreWith(t, func(c *Config) {
		c.Authorize = func(*http.Request, Blob) bool { return true }
	})
	blobID, err := s.Persist(t.Context(), bytes.NewReader(blob(64)), Put{ContentType: "image/png"})
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	row, err := s.index.Get(t.Context(), blobID)
	if err != nil {
		t.Fatalf("row: %v", err)
	}
	if row.Visibility != Private {
		t.Fatalf("stored visibility = %q, want Private", string(row.Visibility))
	}
	rr := serve(t, s, http.MethodGet, blobID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != privateCacheControl {
		t.Fatalf("cache-control = %q, want %q", got, privateCacheControl)
	}
}

// TestPrivateBlobWithoutAuthorizerIsNotFound: a store that cannot admit
// anyone refuses a private blob, and the refusal is indistinguishable
// from an unknown id down to the body.
func TestPrivateBlobWithoutAuthorizerIsNotFound(t *testing.T) {
	s := openTestStore(t)
	blobID, err := s.Persist(t.Context(), bytes.NewReader(blob(64)), Put{ContentType: "image/png"})
	if err != nil {
		t.Fatalf("persist: %v", err)
	}

	rr := serve(t, s, http.MethodGet, blobID, nil)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	if body := rr.Body.String(); strings.Contains(body, blobID) || strings.Contains(body, "private") {
		t.Fatalf("body confirms existence or reason: %q", body)
	}
	if got := rr.Header().Get("Cache-Control"); got != "" {
		t.Fatalf("cache-control = %q, want none on a refusal", got)
	}
}

// TestPersistAndServeFullBody pins the base contract: persist bytes,
// serve them back byte for byte under the stored content type, with the
// immutable caching headers generated media needs.
func TestPersistAndServeFullBody(t *testing.T) {
	s := openTestStore(t)
	data := blob(4096)
	blobID := persistPublic(t, s, "audio/mpeg", data)

	rr := serve(t, s, http.MethodGet, blobID, nil)

	if got, want := rr.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d (body: %s)", got, want, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "audio/mpeg" {
		t.Fatalf("content-type = %q, want audio/mpeg", got)
	}
	if got := rr.Header().Get("Cache-Control"); got != publicCacheControl {
		t.Fatalf("cache-control = %q, want %q", got, publicCacheControl)
	}
	if got := rr.Header().Get("ETag"); got != `"`+blobID+`"` {
		t.Fatalf("etag = %q, want quoted id", got)
	}
	if got := rr.Body.Bytes(); !bytes.Equal(got, data) {
		t.Fatalf("body = %d bytes (crc %d), want %d bytes (crc %d)",
			len(got), crc32.ChecksumIEEE(got), len(data), crc32.ChecksumIEEE(data))
	}
}

// The two cache header pairs the handler sets are named here, so a test
// asserting on them cannot drift from the value another test asserts.
const (
	publicCacheControl  = "public, max-age=31536000, immutable"
	privateCacheControl = "private, no-store"
)

// TestPersistRejectsUnsupportedContentTypes pins the closed set: bytes
// with an unvetted or empty type must never become servable, because
// the handler serves that type verbatim.
func TestPersistRejectsUnsupportedContentTypes(t *testing.T) {
	s := openTestStore(t)
	cases := []struct {
		name        string
		contentType string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"html", "text/html"},
		{"unsupported video", "video/x-matroska"},
		{"invented", "audio/x-jealous-elephant"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			blobID, err := s.Persist(t.Context(), bytes.NewReader(blob(8)), Put{ContentType: c.contentType})
			if !errors.Is(err, ErrInvalidContentType) {
				t.Fatalf("err = %v, want ErrInvalidContentType", err)
			}
			if blobID != "" {
				t.Fatalf("persist returned id %q alongside an error", blobID)
			}
		})
	}
}

// TestPersistAcceptsSupportedTypes pins every type in the closed set as
// accepted. A type silently dropped from the set breaks the model that
// produces it.
func TestPersistAcceptsSupportedTypes(t *testing.T) {
	s := openTestStore(t)
	for _, ct := range defaultContentTypes {
		t.Run(ct, func(t *testing.T) {
			blobID, err := s.Persist(t.Context(), bytes.NewReader(blob(8)), Put{ContentType: ct})
			if err != nil {
				t.Fatalf("persist %q: %v", ct, err)
			}
			row, err := s.index.Get(t.Context(), blobID)
			if err != nil {
				t.Fatalf("row: %v", err)
			}
			if row.ContentType != ct {
				t.Fatalf("row content type = %q, want %q", row.ContentType, ct)
			}
		})
	}
}

// TestPersistNormalizesContentType: parameters and case are stripped, so
// the stored type is the bare media type the handler serves.
func TestPersistNormalizesContentType(t *testing.T) {
	s := openTestStore(t)
	blobID, err := s.Persist(t.Context(), bytes.NewReader(blob(8)), Put{ContentType: "IMAGE/PNG; charset=binary"})
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	row, err := s.index.Get(t.Context(), blobID)
	if err != nil {
		t.Fatalf("row: %v", err)
	}
	if row.ContentType != "image/png" {
		t.Fatalf("content type = %q, want image/png", row.ContentType)
	}
}

// TestPersistIDsAreUnguessableAndDistinct: a media URL is shareable only
// because its id carries 128 bits of entropy and never repeats.
func TestPersistIDsAreUnguessableAndDistinct(t *testing.T) {
	s := openTestStore(t)
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		blobID, err := s.Persist(t.Context(), bytes.NewReader(blob(4)), Put{ContentType: "image/png"})
		if err != nil {
			t.Fatalf("persist: %v", err)
		}
		if !id.Valid(blobID) {
			t.Fatalf("id %q is not a 32 character lowercase hex string", blobID)
		}
		if seen[blobID] {
			t.Fatalf("persist repeated id %q", blobID)
		}
		seen[blobID] = true
	}
}

// TestPersistWithCancelledContextFailsClean: ctx bounds the metadata
// write, and a failed insert leaves no blob file behind, because the
// file only becomes reachable through its row.
func TestPersistWithCancelledContextFailsClean(t *testing.T) {
	s := openTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := s.Persist(ctx, bytes.NewReader(blob(8)), Put{ContentType: "image/png"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	emptyDir(t, s.dir)
}

// TestServeHTTPRangeRequests pins the property that audio scrubbing
// depends on: a Range header on a stored blob returns 206 with exactly
// the requested bytes and the stored content type.
func TestServeHTTPRangeRequests(t *testing.T) {
	s := openTestStore(t)
	data := blob(1024)
	blobID := persistPublic(t, s, "audio/mpeg", data)

	cases := []struct {
		name          string
		rangeHeader   string
		wantStatus    int
		wantSlice     []byte
		wantContRange string
	}{
		{"leading bytes", "bytes=0-0", http.StatusPartialContent, data[0:1], "bytes 0-0/1024"},
		{"middle range", "bytes=2-5", http.StatusPartialContent, data[2:6], "bytes 2-5/1024"},
		{"open ended suffix range", "bytes=1000-", http.StatusPartialContent, data[1000:], "bytes 1000-1023/1024"},
		{"tail range", "bytes=-5", http.StatusPartialContent, data[1019:], "bytes 1019-1023/1024"},
		{"full range", "bytes=0-", http.StatusPartialContent, data, "bytes 0-1023/1024"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := serve(t, s, http.MethodGet, blobID, map[string]string{"Range": c.rangeHeader})

			if got := rr.Code; got != c.wantStatus {
				t.Fatalf("status = %d, want %d", got, c.wantStatus)
			}
			if got := rr.Header().Get("Content-Range"); got != c.wantContRange {
				t.Fatalf("content-range = %q, want %q", got, c.wantContRange)
			}
			if got := rr.Header().Get("Content-Type"); got != "audio/mpeg" {
				t.Fatalf("content-type = %q, want audio/mpeg (the stored type)", got)
			}
			if got := rr.Body.Bytes(); !bytes.Equal(got, c.wantSlice) {
				t.Fatalf("body = %d bytes, want the %d requested bytes",
					len(got), len(c.wantSlice))
			}
		})
	}
}

// TestPersistAndServeVideoMP4_RangeRequest pins the video contract: an
// MP4 blob serves with its own content type, immutable caching, a
// quoted ETag, and working sub-slice and suffix ranges, which is what a
// video element needs to seek.
func TestPersistAndServeVideoMP4_RangeRequest(t *testing.T) {
	testRangeServing(t, "video/mp4")
}

// TestPersistAndServePDF_RangeRequest pins the same contract for a PDF,
// which a reader downloads in ranges and caches.
func TestPersistAndServePDF_RangeRequest(t *testing.T) {
	testRangeServing(t, "application/pdf")
}

// testRangeServing is the one shape both media range tests assert: the
// full body, a sub-slice range, and a suffix range.
func testRangeServing(t *testing.T, contentType string) {
	t.Helper()
	s := openTestStore(t)
	data := blob(8192)
	blobID := persistPublic(t, s, contentType, data)

	cases := []struct {
		name          string
		rangeHeader   string
		wantStatus    int
		wantSlice     []byte
		wantContRange string
	}{
		{"full body", "", http.StatusOK, data, ""},
		{"sub-slice", "bytes=1024-2047", http.StatusPartialContent, data[1024:2048], "bytes 1024-2047/8192"},
		{"suffix", "bytes=-512", http.StatusPartialContent, data[7680:8192], "bytes 7680-8191/8192"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sendHeaders := map[string]string{}
			if c.rangeHeader != "" {
				sendHeaders["Range"] = c.rangeHeader
			}
			rr := serve(t, s, http.MethodGet, blobID, sendHeaders)

			if got := rr.Code; got != c.wantStatus {
				t.Fatalf("status = %d, want %d", got, c.wantStatus)
			}
			if got := rr.Header().Get("Content-Type"); got != contentType {
				t.Fatalf("content-type = %q, want %q", got, contentType)
			}
			if got := rr.Header().Get("Cache-Control"); got != publicCacheControl {
				t.Fatalf("cache-control = %q, want %q", got, publicCacheControl)
			}
			if got := rr.Header().Get("ETag"); got != `"`+blobID+`"` {
				t.Fatalf("etag = %q, want the quoted id", got)
			}
			if got := rr.Header().Get("Content-Range"); got != c.wantContRange {
				t.Fatalf("content-range = %q, want %q", got, c.wantContRange)
			}
			if got := rr.Body.Bytes(); !bytes.Equal(got, c.wantSlice) {
				t.Fatalf("body = %d bytes (crc %d), want %d bytes (crc %d)",
					len(got), crc32.ChecksumIEEE(got), len(c.wantSlice), crc32.ChecksumIEEE(c.wantSlice))
			}
		})
	}
}

// TestServeHTTPIfNoneMatchPins304: media is immutable, so a matching
// ETag answers 304 and saves the bytes entirely.
func TestServeHTTPIfNoneMatchPins304(t *testing.T) {
	s := openTestStore(t)
	blobID := persistPublic(t, s, "image/png", blob(16))
	rr := serve(t, s, http.MethodGet, blobID, map[string]string{"If-None-Match": `"` + blobID + `"`})
	if got, want := rr.Code, http.StatusNotModified; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
}

// TestServeHTTPUnknownAndMalformedIDs: both a well formed id with no row
// and a malformed id are 404. The malformed check is also what keeps a
// crafted path value away from the file layer.
func TestServeHTTPUnknownAndMalformedIDs(t *testing.T) {
	s := openTestStore(t)
	cases := []string{
		"00000000000000000000000000000000", // well formed, no row
		"short",
		"../etc/passwd",
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", // uppercase is not our id shape
		"000000000000000000000000000000zz", // non-hex tail
	}
	for _, blobID := range cases {
		t.Run(blobID, func(t *testing.T) {
			rr := serve(t, s, http.MethodGet, blobID, nil)
			if got, want := rr.Code, http.StatusNotFound; got != want {
				t.Fatalf("status = %d, want %d", got, want)
			}
		})
	}
}

// TestServeHTTPRejectsNonGETMethods pins the method gate on the handler
// itself, not only on the mux route.
func TestServeHTTPRejectsNonGETMethods(t *testing.T) {
	s := openTestStore(t)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rr := serve(t, s, method, "00000000000000000000000000000000", nil)
		if got, want := rr.Code, http.StatusMethodNotAllowed; got != want {
			t.Fatalf("%s status = %d, want %d", method, got, want)
		}
		if got, want := rr.Header().Get("Allow"), "GET, HEAD"; got != want {
			t.Fatalf("allow = %q, want %q", got, want)
		}
	}
}

// TestServeHTTPRowWithoutFileIs404: a row whose file vanished, which is
// what a crash between the row delete and the file remove leaves,
// answers 404. There is nothing else an honest server can say.
func TestServeHTTPRowWithoutFileIs404(t *testing.T) {
	s := openTestStore(t)
	const blobID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	row := Blob{ID: blobID, ContentType: "image/png", SizeBytes: 3, Visibility: Public}
	if err := s.index.Create(t.Context(), row); err != nil {
		t.Fatalf("create row: %v", err)
	}
	rr := serve(t, s, http.MethodGet, blobID, nil)
	if got, want := rr.Code, http.StatusNotFound; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
}

// TestDeleteRemovesRowAndFile: after Delete both the metadata and the
// bytes are gone, and a second delete reports the same missing row
// rather than succeeding silently.
func TestDeleteRemovesRowAndFile(t *testing.T) {
	s := openTestStore(t)
	blobID := persistPublic(t, s, "image/png", blob(8))
	if err := s.Delete(t.Context(), blobID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.index.Get(t.Context(), blobID); !errors.Is(err, errMemNotFound) {
		t.Fatalf("row after delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, blobID)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("file after delete: %v", err)
	}
	if err := s.Delete(t.Context(), blobID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v, want ErrNotFound", err)
	}
}

// TestPersistWithIDAndDeleteIfPresent: a caller that reserves an id can
// land its bytes under it, and can clean up whether or not the blob
// made it to disk.
func TestPersistWithIDAndDeleteIfPresent(t *testing.T) {
	s := openTestStore(t)
	blobID := strings.Repeat("a", 32)
	if err := s.PersistWithID(t.Context(), blobID, bytes.NewReader(blob(8)), Put{ContentType: "audio/mpeg"}); err != nil {
		t.Fatalf("persist with id: %v", err)
	}
	if _, err := s.index.Get(t.Context(), blobID); err != nil {
		t.Fatalf("persist with id row: %v", err)
	}
	if err := s.DeleteIfPresent(t.Context(), blobID); err != nil {
		t.Fatalf("delete if present: %v", err)
	}
	if err := s.DeleteIfPresent(t.Context(), blobID); err != nil {
		t.Fatalf("delete if present on a missing id: %v", err)
	}
	if err := s.PersistWithID(t.Context(), "../escape", bytes.NewReader(blob(8)), Put{ContentType: "audio/mpeg"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed id error = %v, want ErrNotFound", err)
	}
	if err := s.PersistWithID(t.Context(), "not-an-id", bytes.NewReader(blob(8)), Put{ContentType: "audio/mpeg"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("short id error = %v, want ErrNotFound", err)
	}
}

// TestPersistWithIDOnATakenIDReportsTheConflict: an id the index already
// holds is refused, and the bytes the refused attempt wrote are removed
// again, which is the same cleanup any failed insert runs.
func TestPersistWithIDOnATakenIDReportsTheConflict(t *testing.T) {
	s := openTestStore(t)
	taken := strings.Repeat("c", 32)
	row := Blob{ID: taken, ContentType: "image/png", SizeBytes: 3}
	if err := s.index.Create(t.Context(), row); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	err := s.PersistWithID(t.Context(), taken, bytes.NewReader(blob(8)), Put{ContentType: "image/png"})
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("err = %v, want ErrAlreadyExists", err)
	}
	emptyDir(t, s.dir)
}

// TestPersistWithIDRefusedRetryLeavesTheStoredBlob: a second persist for
// an id that already has a blob is refused, and the refusal must not
// truncate or remove the file the first persist stored. The handler must
// keep serving the stored bytes.
func TestPersistWithIDRefusedRetryLeavesTheStoredBlob(t *testing.T) {
	s := openTestStore(t)
	stored := strings.Repeat("b", 32)
	first := blob(100)
	if err := s.PersistWithID(t.Context(), stored, bytes.NewReader(first), Put{
		ContentType: "image/png",
		Visibility:  Public,
	}); err != nil {
		t.Fatalf("first persist: %v", err)
	}

	err := s.PersistWithID(t.Context(), stored, bytes.NewReader(blob(40)), Put{
		ContentType: "image/png",
		Visibility:  Public,
	})
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("retry on a taken id err = %v, want ErrAlreadyExists", err)
	}

	got, readErr := os.ReadFile(filepath.Join(s.dir, stored))
	if readErr != nil {
		t.Fatalf("stored blob file gone after a refused retry: %v", readErr)
	}
	if !bytes.Equal(got, first) {
		t.Fatal("stored blob bytes changed after a refused retry")
	}
	rr := serve(t, s, http.MethodGet, stored, nil)
	if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), first) {
		t.Fatalf("served %d after a refused retry, want 200 with the stored bytes", rr.Code)
	}
}

// TestPersistWithIDRetryAfterACleanedFailureSucceeds: the id is reserved
// before its row exists, so an attempt that fails and removes its own
// file must leave the id retryable.
func TestPersistWithIDRetryAfterACleanedFailureSucceeds(t *testing.T) {
	s := openTestStore(t)
	reserved := strings.Repeat("e", 32)
	dead, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.PersistWithID(dead, reserved, bytes.NewReader(blob(8)), Put{ContentType: "audio/mpeg"}); err == nil {
		t.Fatal("persist survived a cancelled context")
	}
	emptyDir(t, s.dir)

	if err := s.PersistWithID(t.Context(), reserved, bytes.NewReader(blob(8)), Put{ContentType: "audio/mpeg"}); err != nil {
		t.Fatalf("retry on a reserved id: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, reserved)); err != nil {
		t.Fatalf("retried blob file missing: %v", err)
	}
}

// TestOpenValidations: a store without a directory, without an index, or
// with a content type entry that normalizes to nothing is unusable and
// says so.
func TestOpenValidations(t *testing.T) {
	ctx := t.Context()
	if _, err := Open(ctx, Config{Index: newMemIndex()}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty dir err = %v, want ErrInvalid", err)
	}
	if _, err := Open(ctx, Config{Dir: t.TempDir()}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil index err = %v, want ErrInvalid", err)
	}
	cfg := Config{Dir: t.TempDir(), Index: newMemIndex(), ContentTypes: []string{"image/png", "   "}}
	if _, err := Open(ctx, cfg); !errors.Is(err, ErrInvalid) {
		t.Fatalf("blank content type err = %v, want ErrInvalid", err)
	}
}

// TestOpenCreatesDir: first boot brings its own blob directory up,
// including the parents that do not exist yet.
func TestOpenCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "media")
	s, err := Open(t.Context(), Config{Dir: dir, Index: newMemIndex()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.root.Close()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir after open: %v", err)
	}
}

// midStreamError fails after delivering n bytes. A truncated source must
// fail the persist rather than store a partial blob.
type midStreamError struct{ n int }

func (m *midStreamError) Read(p []byte) (int, error) {
	if m.n <= 0 {
		return 0, errors.New("source died")
	}
	give := min(m.n, len(p))
	for i := range give {
		p[i] = byte(i)
	}
	m.n -= give
	return give, nil
}

// TestPersistWithTruncatedSourceFailsWithoutOrphans: a read error
// mid-copy aborts the persist and leaves neither row nor file.
func TestPersistWithTruncatedSourceFailsWithoutOrphans(t *testing.T) {
	s := openTestStore(t)
	_, err := s.Persist(t.Context(), &midStreamError{n: 4}, Put{ContentType: "image/png"})
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want the copy failure", err)
	}
	emptyDir(t, s.dir)
}

// TestPersistIntoReadOnlyDirFails: when the blob file cannot be created
// the persist fails instead of pretending. Skipped under root, which
// ignores directory permissions.
func TestPersistIntoReadOnlyDirFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	s := openTestStore(t)
	if err := os.Chmod(s.dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(s.dir, 0o755) })
	if _, err := s.Persist(t.Context(), bytes.NewReader(blob(8)), Put{ContentType: "image/png"}); err == nil {
		t.Fatal("persist into a read-only directory succeeded")
	}
}

// TestOpenOnAFileFails: a directory path that is actually a regular file
// cannot host blobs.
func TestOpenOnAFileFails(t *testing.T) {
	file := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.Context(), Config{Dir: file, Index: newMemIndex()}); err == nil {
		t.Fatal("open on a regular file succeeded")
	}
}

// capture is a slog handler recording record messages. The operator-log
// branches are observable only through them.
type capture struct {
	mu       sync.Mutex
	messages []string
}

func (c *capture) Enabled(context.Context, slog.Level) bool { return true }

func (c *capture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, r.Message)
	return nil
}

func (c *capture) WithAttrs([]slog.Attr) slog.Handler { return c }

func (c *capture) WithGroup(string) slog.Handler { return c }

func (c *capture) has(message string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.messages {
		if m == message {
			return true
		}
	}
	return false
}

// failingBlob serves real writes through an underlying file while
// failing the stages a test injects. It is the fault injection behind
// the sync and close failure pins.
type failingBlob struct {
	*os.File
	failSync  error
	failClose error
}

func (f *failingBlob) Sync() error {
	if f.failSync != nil {
		return f.failSync
	}
	return f.File.Sync()
}

func (f *failingBlob) Close() error {
	if f.failClose != nil {
		return f.failClose
	}
	return f.File.Close()
}

// emptyDir fails the test unless the blob directory holds nothing, which
// is the observable of "the partial file was removed".
func emptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		t.Fatalf("blob dir holds %q after the failed persist, want it removed", e.Name())
	}
}

// TestNotFoundSentinelContract: every not-found path matches the
// package sentinel, and every one that originates in a missing row
// matches the index's own sentinel too, so a consumer following either
// contract classifies correctly.
func TestNotFoundSentinelContract(t *testing.T) {
	s := openTestStore(t)
	const unknown = "ffffffffffffffffffffffffffffffff"
	cases := []struct {
		name          string
		probe         func(t *testing.T) error
		matchIndexToo bool
	}{
		{"delete unknown id", func(t *testing.T) error {
			return s.Delete(t.Context(), unknown)
		}, true},
		{"delete malformed id", func(t *testing.T) error {
			return s.Delete(t.Context(), "../secrets")
		}, false},
		{"find unknown id", func(t *testing.T) error {
			_, err := s.find(t.Context(), unknown)
			return err
		}, true},
		{"find malformed id", func(t *testing.T) error {
			_, err := s.find(t.Context(), "0x")
			return err
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.probe(t)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("err = %v, want a match on ErrNotFound", err)
			}
			if c.matchIndexToo && !errors.Is(err, errMemNotFound) {
				t.Fatalf("err = %v, want a match on the index's missing-row error too", err)
			}
		})
	}
}

// TestServeHTTPWithClosedIndexIs500: a metadata lookup that fails behind
// the handler is a server fault, not a 404. The documented answer is a
// 500 and one log line. Deleting the 500 branch turns this pin red,
// because the request would ride on and serve the file as if nothing
// were wrong.
func TestServeHTTPWithClosedIndexIs500(t *testing.T) {
	s := openTestStore(t)
	logs := &capture{}
	s.log = slog.New(logs)
	blobID := persistPublic(t, s, "image/png", blob(8))
	s.index.(*memIndex).shut()

	rr := serve(t, s, http.MethodGet, blobID, nil)
	if got, want := rr.Code, http.StatusInternalServerError; got != want {
		t.Fatalf("status = %d, want %d (body: %s)", got, want, rr.Body.String())
	}
	if !logs.has("mediastore: metadata lookup failed") {
		t.Fatal("no metadata-lookup-failed line was logged")
	}
}

// TestServeHTTPUnopenableBlobIs500: a row whose file exists but cannot
// be opened is a server fault, not a 404. The client did nothing wrong,
// and the documented answer is a 500 and one log line. The log line is
// load-bearing, because the content helper happens to answer 500 on an
// unusable handle anyway. Skipped under root, which ignores file
// permissions.
func TestServeHTTPUnopenableBlobIs500(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	s := openTestStore(t)
	logs := &capture{}
	s.log = slog.New(logs)
	blobID := persistPublic(t, s, "image/png", blob(8))
	if err := os.Chmod(filepath.Join(s.dir, blobID), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(s.dir, blobID), 0o644) })

	rr := serve(t, s, http.MethodGet, blobID, nil)
	if got, want := rr.Code, http.StatusInternalServerError; got != want {
		t.Fatalf("status = %d, want %d (body: %s)", got, want, rr.Body.String())
	}
	if !logs.has("mediastore: blob open failed") {
		t.Fatal("no blob-open-failed line was logged")
	}
}

// TestPersistSyncFailureRemovesPartialBlob: the fsync stage failing, as
// a full disk or a dying volume makes it, must abort the persist and
// leave neither partial file nor row behind.
func TestPersistSyncFailureRemovesPartialBlob(t *testing.T) {
	s := openTestStore(t)
	s.newBlob = func(blobID string) (blobFile, error) {
		f, err := s.root.Create(blobID)
		if err != nil {
			return nil, err
		}
		return &failingBlob{File: f, failSync: errors.New("disk refuses to flush")}, nil
	}
	_, err := s.Persist(t.Context(), bytes.NewReader(blob(64)), Put{ContentType: "image/png"})
	if err == nil || !strings.Contains(err.Error(), "sync blob") {
		t.Fatalf("err = %v, want the sync stage failure", err)
	}
	emptyDir(t, s.dir)
}

// TestPersistCloseFailureRemovesPartialBlob: the close stage failing
// after a successful write and sync must abort the persist too, because
// the blob is not trusted on disk until Close agrees.
func TestPersistCloseFailureRemovesPartialBlob(t *testing.T) {
	s := openTestStore(t)
	s.newBlob = func(blobID string) (blobFile, error) {
		f, err := s.root.Create(blobID)
		if err != nil {
			return nil, err
		}
		return &failingBlob{File: f, failClose: errors.New("close refuses")}, nil
	}
	_, err := s.Persist(t.Context(), bytes.NewReader(blob(64)), Put{ContentType: "image/png"})
	if err == nil || !strings.Contains(err.Error(), "close blob") {
		t.Fatalf("err = %v, want the close stage failure", err)
	}
	emptyDir(t, s.dir)
}

// TestPersistSyncFailureWithFailingRemoveLogsOrphan: when the sync fails
// and the partial file cannot be removed either, the remove failure is
// one log line for the operator rather than a swallowed error. Skipped
// under root, which ignores directory permissions.
func TestPersistSyncFailureWithFailingRemoveLogsOrphan(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	s := openTestStore(t)
	logs := &capture{}
	s.log = slog.New(logs)
	s.newBlob = func(blobID string) (blobFile, error) {
		f, err := s.root.Create(blobID)
		if err != nil {
			return nil, err
		}
		// The blob file is on disk, so make the directory
		// unremovable-from before the injected sync failure sends the
		// cleanup closure after it.
		if err := os.Chmod(s.dir, 0o555); err != nil {
			return nil, err
		}
		return &failingBlob{File: f, failSync: errors.New("disk refuses to flush")}, nil
	}
	t.Cleanup(func() { os.Chmod(s.dir, 0o755) })
	if _, err := s.Persist(t.Context(), bytes.NewReader(blob(64)), Put{ContentType: "image/png"}); err == nil {
		t.Fatal("persist survived a sync failure")
	}
	if !logs.has("mediastore: partial blob not removed") {
		t.Fatal("no partial-blob-not-removed line was logged")
	}
}
