package upload

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"
)

// serve starts a real HTTP server over the handler and returns the URL its
// routes hang off.
func (x *harness) serve() string {
	x.t.Helper()
	mux := http.NewServeMux()
	x.h.Mount(mux)
	srv := httptest.NewServer(mux)
	x.t.Cleanup(srv.Close)
	return srv.URL + x.h.base
}

// send makes one request with a client that holds nothing between calls,
// and returns the status and the body it read.
func send(t *testing.T, method, url string, body []byte, header ...string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response: %v", err)
		}
	}()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, got
}

// TestUploadResumesAfterAnInterruption shows a client that drops out
// mid-upload can be replaced by one that knows only the id: the server says
// which chunks arrived, and the new client sends exactly what is missing.
func TestUploadResumesAfterAnInterruption(t *testing.T) {
	x := newHarness(t, Config{})
	base := x.serve()
	up := x.begin("owner-1", "video/mp4", size(int64(len(whole()))), int64(len(chunks[0])))

	// The first client sends two of the three chunks, out of order, and
	// goes away without finishing.
	for _, index := range []int{0, 2} {
		status, body := send(t, http.MethodPut, base+"/"+up.ID+"/chunks/"+strconv.Itoa(index), chunks[index], chunkDigestHeader, digest(chunks[index]))
		if status != http.StatusOK {
			t.Fatalf("chunk %d: status %d: %s", index, status, body)
		}
	}

	// The second client holds nothing but the id.
	status, body := send(t, http.MethodGet, base+"/"+up.ID, nil)
	if status != http.StatusOK {
		t.Fatalf("state: status %d: %s", status, body)
	}
	var state stateResponse
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %q: %v", body, err)
	}
	if !slices.Equal(state.Received, []int{0, 2}) {
		t.Fatalf("received %v, want [0 2]", state.Received)
	}
	if !slices.Equal(state.Missing, []int{1}) {
		t.Fatalf("missing %v, want [1]", state.Missing)
	}
	if state.StoredBytes != int64(len(chunks[0])+len(chunks[2])) {
		t.Fatalf("stored %d bytes, want %d", state.StoredBytes, len(chunks[0])+len(chunks[2]))
	}

	// It sends the chunk the server named and finishes the file.
	for _, index := range state.Missing {
		status, body := send(t, http.MethodPut, base+"/"+up.ID+"/chunks/"+strconv.Itoa(index), chunks[index], chunkDigestHeader, digest(chunks[index]))
		if status != http.StatusOK {
			t.Fatalf("chunk %d: status %d: %s", index, status, body)
		}
	}
	request, err := json.Marshal(completeRequest{SHA256: digest(whole())})
	if err != nil {
		t.Fatalf("marshal completion: %v", err)
	}
	status, body = send(t, http.MethodPost, base+"/"+up.ID+"/complete", request)
	if status != http.StatusCreated {
		t.Fatalf("complete: status %d: %s", status, body)
	}
	if !bytes.Equal(x.store.blob(up.ID), whole()) {
		t.Fatalf("stored bytes do not match the uploaded file")
	}
}

// TestUploadExpiresWhenItIsAbandoned shows an upload that stops receiving
// chunks is served until its time to live runs out, and is gone after it:
// the index entry answers not_found and the sweep takes the files away.
func TestUploadExpiresWhenItIsAbandoned(t *testing.T) {
	x := newHarness(t, Config{UploadTTL: time.Hour})
	up := x.begin("owner-1", "video/mp4", size(int64(len(whole()))), int64(len(chunks[0])))
	x.putOK(up.ID, 0, chunks[0])
	if files := x.uploadFiles(up.ID); len(files) != 1 {
		t.Fatalf("staged files %v, want the chunk that arrived", files)
	}

	x.clock.advance(time.Hour - time.Second)
	if state := x.state(up.ID); state.StoredBytes != int64(len(chunks[0])) {
		t.Fatalf("state just before expiry %+v", state)
	}

	x.clock.advance(2 * time.Second)
	x.refuse(x.call(http.MethodGet, x.uploadPath(up.ID), nil, nil), http.StatusNotFound, codeNotFound)

	removed, err := x.h.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 1 {
		t.Fatalf("swept %d uploads, want 1", removed)
	}
	if files := x.uploadFiles(up.ID); len(files) != 0 {
		t.Fatalf("an expired upload left %v on disk", files)
	}
	if names := x.stagingNames(); len(names) != 0 {
		t.Fatalf("the staging directory still holds %v", names)
	}
}

// TestSweepRemovesChunksLeftByAnEarlierRun shows a staging directory no
// index entry claims is removed once it is older than the time to live,
// which is what makes a restart leave no debris behind.
func TestSweepRemovesChunksLeftByAnEarlierRun(t *testing.T) {
	x := newHarness(t, Config{UploadTTL: time.Hour})
	leftover := filepath.Join(x.dir, "ffffffffffffffffffffffffffffffff")
	if err := os.MkdirAll(leftover, 0o700); err != nil {
		t.Fatalf("make staging directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(leftover, "0.chunk"), []byte("from an earlier process"), 0o600); err != nil {
		t.Fatalf("write chunk: %v", err)
	}
	old := x.clock.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(leftover, old, old); err != nil {
		t.Fatalf("age staging directory: %v", err)
	}

	removed, err := x.h.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 1 {
		t.Fatalf("swept %d directories, want 1", removed)
	}
	if names := x.stagingNames(); len(names) != 0 {
		t.Fatalf("the staging directory still holds %v", names)
	}
}

// TestStartSweepsExpiredUploads shows the loop Start runs removes an upload
// that aged out with nobody asking, and that Close stops the loop.
func TestStartSweepsExpiredUploads(t *testing.T) {
	x := newHarness(t, Config{UploadTTL: time.Minute, SweepInterval: time.Millisecond})
	up := x.begin("owner-1", "video/mp4", size(int64(len(whole()))), int64(len(chunks[0])))
	x.putOK(up.ID, 0, chunks[0])

	x.h.Start()
	x.clock.advance(2 * time.Minute)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(x.dir, up.ID)); errors.Is(err, fs.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the upload was not swept by the running loop")
		}
		time.Sleep(time.Millisecond)
	}
	if err := x.h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestUploadRefusesBytesOverTheUploadLimit shows the per-upload limit
// refuses a chunk while it arrives, names itself in the refusal, and leaves
// the chunks that fit alone.
func TestUploadRefusesBytesOverTheUploadLimit(t *testing.T) {
	x := newHarness(t, Config{MaxUploadBytes: 10, MaxOwnerBytes: 1 << 20, MaxChunks: 64})
	up := x.begin("owner-1", "video/mp4", nil, 8)
	x.putOK(up.ID, 0, []byte("12345678"))

	over := []byte("abcdefgh")
	got := x.refuse(x.put(up.ID, 1, over, digest(over)), http.StatusRequestEntityTooLarge, codeLimitExceeded)
	var detail limitDetail
	x.detail(got, &detail)
	if detail.Limit != "upload" {
		t.Fatalf("limit %q, want %q", detail.Limit, "upload")
	}
	if detail.Max != 10 || detail.Total <= detail.Max {
		t.Fatalf("refusal detail %+v", detail)
	}
	if state := x.state(up.ID); state.StoredBytes != 8 {
		t.Fatalf("state %+v, want the chunk that fit to be untouched", state)
	}
	if files := x.uploadFiles(up.ID); len(files) != 1 {
		t.Fatalf("staged files %v, want only the chunk that fit", files)
	}
}

// TestUploadRefusesBytesOverTheOwnerLimit shows the per-owner limit refuses
// a chunk that would push one owner past its budget, names itself in the
// refusal, and leaves another owner free to upload.
func TestUploadRefusesBytesOverTheOwnerLimit(t *testing.T) {
	x := newHarness(t, Config{MaxUploadBytes: 1 << 20, MaxOwnerBytes: 12, MaxChunks: 1 << 20})
	first := x.begin("owner-1", "video/mp4", nil, 8)
	x.putOK(first.ID, 0, []byte("12345678"))

	second := x.begin("owner-1", "video/mp4", nil, 8)
	over := []byte("abcdefgh")
	got := x.refuse(x.put(second.ID, 0, over, digest(over)), http.StatusRequestEntityTooLarge, codeLimitExceeded)
	var detail limitDetail
	x.detail(got, &detail)
	if detail.Limit != "owner" {
		t.Fatalf("limit %q, want %q", detail.Limit, "owner")
	}
	if detail.Max != 12 || detail.Total <= detail.Max {
		t.Fatalf("refusal detail %+v", detail)
	}

	other := x.begin("owner-2", "video/mp4", nil, 8)
	x.putOK(other.ID, 0, over)
}

// TestUploadRefusesAStartOverItsLimits shows an upload that declares more
// bytes than a limit allows is refused before any chunk can arrive, so the
// refusal costs no disk.
func TestUploadRefusesAStartOverItsLimits(t *testing.T) {
	x := newHarness(t, Config{MaxUploadBytes: 4096, MaxOwnerBytes: 2048, MaxChunks: 64})

	rec := x.postJSON(x.h.base, startRequest{Owner: "owner-1", ContentType: "video/mp4", SizeBytes: size(8192), ChunkSize: 512})
	got := x.refuse(rec, http.StatusRequestEntityTooLarge, codeLimitExceeded)
	var detail limitDetail
	x.detail(got, &detail)
	if detail.Limit != "upload" || detail.Max != 4096 || detail.Total != 8192 {
		t.Fatalf("refusal detail %+v", detail)
	}

	held := make([]byte, 512)
	first := x.begin("owner-1", "video/mp4", size(512), 512)
	x.putOK(first.ID, 0, held)

	rec = x.postJSON(x.h.base, startRequest{Owner: "owner-1", ContentType: "video/mp4", SizeBytes: size(1900), ChunkSize: 512})
	got = x.refuse(rec, http.StatusRequestEntityTooLarge, codeLimitExceeded)
	x.detail(got, &detail)
	if detail.Limit != "owner" || detail.Max != 2048 || detail.Total != 2412 {
		t.Fatalf("refusal detail %+v", detail)
	}
	if names := x.stagingNames(); len(names) != 1 {
		t.Fatalf("staging directory holds %v, want only the upload that arrived", names)
	}
}
