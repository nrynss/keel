package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nrynss/keel/mediastore"
)

// fakeStore records the blobs a handler persists, in memory.
type fakeStore struct {
	mu   sync.Mutex
	data map[string][]byte
	meta map[string]mediastore.Put
	err  error
}

// newFakeStore returns an empty recording store.
func newFakeStore() *fakeStore {
	return &fakeStore{data: make(map[string][]byte), meta: make(map[string]mediastore.Put)}
}

// PersistWithID implements BlobStore.
func (s *fakeStore) PersistWithID(_ context.Context, blobID string, src io.Reader, p mediastore.Put) error {
	body, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if _, ok := s.data[blobID]; ok {
		return mediastore.ErrAlreadyExists
	}
	s.data[blobID] = body
	s.meta[blobID] = p
	return nil
}

// blob returns the bytes stored under id.
func (s *fakeStore) blob(id string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[id]
}

// metadata returns the metadata stored under id.
func (s *fakeStore) metadata(id string) mediastore.Put {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta[id]
}

// seed stores bytes under id before the handler runs, so a test can make
// the next id collide.
func (s *fakeStore) seed(id string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[id] = body
}

// failWith makes every later persist fail with err, and nil clears it.
func (s *fakeStore) failWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// fakeClock is a clock a test moves by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// Now reports the clock's current reading.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// advance moves the clock forward.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// harness is one handler over a fresh staging directory, a recording store
// and a clock the test moves.
type harness struct {
	t     *testing.T
	h     *Handler
	store *fakeStore
	clock *fakeClock
	dir   string
	ids   int
}

// newHarness builds a harness whose ids count up from one, so a recorded
// response is reproducible. A field left zero in cfg is filled from the
// harness.
func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	x := &harness{
		t:     t,
		store: newFakeStore(),
		clock: &fakeClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		dir:   t.TempDir(),
	}
	cfg.Dir = x.dir
	if cfg.Store == nil {
		cfg.Store = x.store
	}
	if cfg.Now == nil {
		cfg.Now = x.clock.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	h, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.newID = x.nextID
	x.h = h
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return x
}

// nextID mints ids that count up from one.
func (x *harness) nextID() (string, error) {
	x.ids++
	return fmt.Sprintf("%032x", x.ids), nil
}

// call serves one request through the handler and returns the recorder. An
// absent body is sent as an empty one, so the handler always finds a body
// to read.
func (x *harness) call(method, target string, body []byte, header map[string]string) *httptest.ResponseRecorder {
	x.t.Helper()
	if body == nil {
		body = []byte{}
	}
	r := httptest.NewRequest(method, target, bytes.NewReader(body))
	for name, value := range header {
		r.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)
	return rec
}

// uploadPath is the path of one upload.
func (x *harness) uploadPath(id string) string {
	return x.h.base + "/" + id
}

// chunkPath is the path of one chunk.
func (x *harness) chunkPath(id string, index int) string {
	return x.uploadPath(id) + "/chunks/" + strconv.Itoa(index)
}

// postJSON serves a POST carrying v as a JSON body.
func (x *harness) postJSON(target string, v any) *httptest.ResponseRecorder {
	x.t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		x.t.Fatalf("marshal request: %v", err)
	}
	return x.call(http.MethodPost, target, body, map[string]string{"Content-Type": "application/json"})
}

// decode unmarshals a recorder body into v.
func (x *harness) decode(rec *httptest.ResponseRecorder, v any) {
	x.t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		x.t.Fatalf("decode body %q: %v", rec.Body, err)
	}
}

// begin opens an upload and fails the test on a refusal.
func (x *harness) begin(owner, contentType string, size *int64, chunkSize int64) stateResponse {
	x.t.Helper()
	rec := x.postJSON(x.h.base, startRequest{Owner: owner, ContentType: contentType, SizeBytes: size, ChunkSize: chunkSize})
	if rec.Code != http.StatusCreated {
		x.t.Fatalf("begin: status %d: %s", rec.Code, rec.Body)
	}
	var out stateResponse
	x.decode(rec, &out)
	return out
}

// put sends one chunk with the digest declared for it.
func (x *harness) put(id string, index int, body []byte, declared string) *httptest.ResponseRecorder {
	x.t.Helper()
	return x.call(http.MethodPut, x.chunkPath(id, index), body, map[string]string{chunkDigestHeader: declared})
}

// putOK sends one chunk and fails the test unless the handler stores it.
func (x *harness) putOK(id string, index int, body []byte) stateResponse {
	x.t.Helper()
	rec := x.put(id, index, body, digest(body))
	if rec.Code != http.StatusOK {
		x.t.Fatalf("put chunk %d: status %d: %s", index, rec.Code, rec.Body)
	}
	var out stateResponse
	x.decode(rec, &out)
	return out
}

// state asks for an upload's state and fails the test unless it is served.
func (x *harness) state(id string) stateResponse {
	x.t.Helper()
	rec := x.call(http.MethodGet, x.uploadPath(id), nil, nil)
	if rec.Code != http.StatusOK {
		x.t.Fatalf("state: status %d: %s", rec.Code, rec.Body)
	}
	var out stateResponse
	x.decode(rec, &out)
	return out
}

// complete asks the handler to finish an upload.
func (x *harness) complete(id string, sha string) *httptest.ResponseRecorder {
	x.t.Helper()
	return x.postJSON(x.uploadPath(id)+"/complete", completeRequest{SHA256: sha})
}

// completeOK finishes an upload and fails the test unless it lands.
func (x *harness) completeOK(id string, sha string) completeResponse {
	x.t.Helper()
	rec := x.complete(id, sha)
	if rec.Code != http.StatusCreated {
		x.t.Fatalf("complete: status %d: %s", rec.Code, rec.Body)
	}
	var out completeResponse
	x.decode(rec, &out)
	return out
}

// refusal is the error envelope a refusal carries, with its detail left for
// the caller to decode.
type refusal struct {
	Error struct {
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Detail  json.RawMessage `json:"detail"`
	} `json:"error"`
}

// refuse decodes a recorder as a refusal and fails the test when the status
// or the envelope code differs from what the caller expects.
func (x *harness) refuse(rec *httptest.ResponseRecorder, status int, code string) refusal {
	x.t.Helper()
	if rec.Code != status {
		x.t.Fatalf("status %d, want %d: %s", rec.Code, status, rec.Body)
	}
	var out refusal
	x.decode(rec, &out)
	if out.Error.Code != code {
		x.t.Fatalf("code %q, want %q: %s", out.Error.Code, code, rec.Body)
	}
	return out
}

// detail decodes a refusal's detail into v.
func (x *harness) detail(r refusal, v any) {
	x.t.Helper()
	if err := json.Unmarshal(r.Error.Detail, v); err != nil {
		x.t.Fatalf("decode detail %s: %v", r.Error.Detail, err)
	}
}

// stagingNames lists the entries under the staging directory, which is what
// a partial upload leaves behind.
func (x *harness) stagingNames() []string {
	x.t.Helper()
	entries, err := os.ReadDir(x.dir)
	if err != nil {
		x.t.Fatalf("read staging directory: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// uploadFiles lists the files staged for one upload: a fresh look at the
// disk rather than at the index.
func (x *harness) uploadFiles(id string) []string {
	x.t.Helper()
	entries, err := os.ReadDir(filepath.Join(x.dir, id))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		x.t.Fatalf("read upload directory: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// digest is the SHA-256 of data in hex.
func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// size returns a pointer to a copy of n, for a request that declares one.
func size(n int64) *int64 {
	return &n
}
