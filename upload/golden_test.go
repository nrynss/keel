package upload

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// goldenConfig is the handler the recorded protocol was produced by. Every
// field is set, so a change to a default shows up as a golden that no
// longer matches.
func goldenConfig() Config {
	return Config{
		BasePath:       "/uploads",
		MaxUploadBytes: 4096,
		MaxOwnerBytes:  8192,
		MaxChunks:      64,
		UploadTTL:      time.Hour,
	}
}

// goldenStep is one request in the recorded protocol. A step with no file
// only advances the upload, because another step records the same answer.
type goldenStep struct {
	name   string
	golden string
	status int
	do     func() *httptest.ResponseRecorder
}

// goldenSequence drives one upload from its start to its completion in the
// order a client would, returning each request beside the file recorded for
// it. The harness mints ids that count up, so every response body is
// reproducible.
func goldenSequence(x *harness) []goldenStep {
	var id string
	digestOf := func(index int) string { return digest(chunks[index]) }
	return []goldenStep{
		{
			name:   "begin",
			golden: "upload-begin.json",
			status: http.StatusCreated,
			do: func() *httptest.ResponseRecorder {
				rec := x.postJSON(x.h.base, startRequest{
					Owner:       "owner-1",
					ContentType: "video/mp4",
					Visibility:  "private",
					SizeBytes:   size(int64(len(whole()))),
					ChunkSize:   int64(len(chunks[0])),
				})
				var up stateResponse
				x.decode(rec, &up)
				id = up.ID
				return rec
			},
		},
		{
			name:   "chunk",
			golden: "upload-chunk.json",
			status: http.StatusOK,
			do:     func() *httptest.ResponseRecorder { return x.put(id, 0, chunks[0], digestOf(0)) },
		},
		{
			name:   "chunk out of order",
			status: http.StatusOK,
			do:     func() *httptest.ResponseRecorder { return x.put(id, 2, chunks[2], digestOf(2)) },
		},
		{
			name:   "chunk sent again",
			status: http.StatusOK,
			do:     func() *httptest.ResponseRecorder { return x.put(id, 0, chunks[0], digestOf(0)) },
		},
		{
			name:   "state",
			golden: "upload-state.json",
			status: http.StatusOK,
			do:     func() *httptest.ResponseRecorder { return x.call(http.MethodGet, x.uploadPath(id), nil, nil) },
		},
		{
			name:   "complete too early",
			golden: "upload-incomplete.json",
			status: http.StatusConflict,
			do:     func() *httptest.ResponseRecorder { return x.complete(id, digest(whole())) },
		},
		{
			name:   "chunk mismatch",
			golden: "upload-chunk-mismatch.json",
			status: http.StatusConflict,
			do: func() *httptest.ResponseRecorder {
				return x.put(id, 0, chunks[0], strings.Repeat("a", sha256HexSize))
			},
		},
		{
			name:   "last chunk",
			status: http.StatusOK,
			do:     func() *httptest.ResponseRecorder { return x.put(id, 1, chunks[1], digestOf(1)) },
		},
		{
			name:   "hash mismatch",
			golden: "upload-hash-mismatch.json",
			status: http.StatusConflict,
			do: func() *httptest.ResponseRecorder {
				return x.complete(id, strings.Repeat("b", sha256HexSize))
			},
		},
		{
			name:   "complete",
			golden: "upload-complete.json",
			status: http.StatusCreated,
			do:     func() *httptest.ResponseRecorder { return x.complete(id, digest(whole())) },
		},
		{
			name:   "unknown upload",
			golden: "upload-not-found.json",
			status: http.StatusNotFound,
			do: func() *httptest.ResponseRecorder {
				return x.call(http.MethodGet, x.uploadPath("ffffffffffffffffffffffffffffffff"), nil, nil)
			},
		},
		{
			name:   "over the upload byte limit",
			golden: "upload-limit-exceeded.json",
			status: http.StatusRequestEntityTooLarge,
			do: func() *httptest.ResponseRecorder {
				return x.postJSON(x.h.base, startRequest{
					Owner:       "owner-1",
					ContentType: "video/mp4",
					SizeBytes:   size(goldenConfig().MaxUploadBytes + 1),
					ChunkSize:   goldenConfig().MaxUploadBytes,
				})
			},
		},
		{
			name:   "unreadable body",
			golden: "upload-invalid-request.json",
			status: http.StatusBadRequest,
			do:     func() *httptest.ResponseRecorder { return x.call(http.MethodPost, x.h.base, []byte("not json"), nil) },
		},
	}
}

// TestUploadWireGoldens shows every answer this handler gives a client is
// the answer recorded under testdata/wire, byte for byte.
func TestUploadWireGoldens(t *testing.T) {
	x := newHarness(t, goldenConfig())
	for _, step := range goldenSequence(x) {
		t.Run(step.name, func(t *testing.T) {
			rec := step.do()
			if rec.Code != step.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, step.status, rec.Body)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type %q, want application/json", ct)
			}
			if step.golden == "" {
				return
			}
			want := readGolden(t, step.golden)
			if !bytes.Equal(rec.Body.Bytes(), want) {
				t.Fatalf("body does not match %s\n got: %s\nwant: %s", step.golden, rec.Body, want)
			}
		})
	}
}

// readGolden returns the bytes of one protocol file under testdata/wire.
func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("..", "testdata", "wire", name))
	if err != nil {
		t.Fatalf("read golden file: %v", err)
	}
	return want
}
