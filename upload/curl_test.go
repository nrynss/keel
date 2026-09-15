package upload

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// requireCurl skips a test when curl is absent. This test exists to drive
// the handler through a real HTTP client and its own reporting, so a
// library stub would defeat the point.
func requireCurl(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
}

// curlServer starts a real HTTP server in front of the handler and returns
// the URL its routes hang off.
func curlServer(t *testing.T, x *harness) string {
	t.Helper()
	mux := http.NewServeMux()
	x.h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + x.h.base
}

// runCurl runs curl and returns its stdout, failing with curl's own stderr
// on a non-zero exit.
func runCurl(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "curl", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("curl %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
}

// curlSend sends bytes as the body of one request for url and returns the
// status line and the body curl reported, so the caller reads what a shell
// user would.
func curlSend(t *testing.T, url string, body []byte, args ...string) (int, []byte) {
	t.Helper()
	dir := t.TempDir()
	bodyPath := filepath.Join(dir, "request")
	if err := os.WriteFile(bodyPath, body, 0o600); err != nil {
		t.Fatalf("write request body: %v", err)
	}
	outPath := filepath.Join(dir, "response")
	full := append([]string{"-sS", "--data-binary", "@" + bodyPath, "-o", outPath, "-w", "%{http_code}"}, args...)
	stdout := runCurl(t, append(full, url)...)
	status, err := strconv.Atoi(strings.TrimSpace(stdout))
	if err != nil {
		t.Fatalf("curl reported status %q: %v", stdout, err)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return status, got
}

// TestCurlDrivesAWholeUpload runs the protocol through curl against a real
// server, reading the answers out of curl's own reporting: the status, the
// body, and the code inside a refusal.
func TestCurlDrivesAWholeUpload(t *testing.T) {
	requireCurl(t)
	x := newHarness(t, goldenConfig())
	base := curlServer(t, x)
	jsonHeader := []string{"-H", "Content-Type: application/json"}

	start, err := json.Marshal(startRequest{
		Owner:       "owner-1",
		ContentType: "video/mp4",
		SizeBytes:   size(int64(len(whole()))),
		ChunkSize:   int64(len(chunks[0])),
	})
	if err != nil {
		t.Fatalf("marshal start: %v", err)
	}
	status, body := curlSend(t, base, start, append([]string{"-X", "POST"}, jsonHeader...)...)
	if status != http.StatusCreated {
		t.Fatalf("begin: status %d: %s", status, body)
	}
	var up stateResponse
	if err := json.Unmarshal(body, &up); err != nil {
		t.Fatalf("decode begin %q: %v", body, err)
	}
	if up.ID == "" || up.ChunkCount == nil || *up.ChunkCount != len(chunks) {
		t.Fatalf("begin answered %+v", up)
	}

	chunk := func(index int, data []byte, declared string) (int, []byte) {
		t.Helper()
		return curlSend(t, base+"/"+up.ID+"/chunks/"+strconv.Itoa(index), data,
			"-X", "PUT", "-H", chunkDigestHeader+": "+declared)
	}
	for index, data := range chunks {
		status, body := chunk(index, data, digest(data))
		if status != http.StatusOK {
			t.Fatalf("chunk %d: status %d: %s", index, status, body)
		}
	}
	if status, body := chunk(0, chunks[0], digest(chunks[0])); status != http.StatusOK {
		t.Fatalf("repeated chunk: status %d: %s", status, body)
	}

	corrupt := bytes.Clone(chunks[0])
	corrupt[0] ^= 0xff
	status, body = chunk(0, corrupt, digest(chunks[0]))
	if status != http.StatusConflict {
		t.Fatalf("corrupt chunk: status %d: %s", status, body)
	}
	var refusal struct {
		Error struct {
			Code   string         `json:"code"`
			Detail mismatchDetail `json:"detail"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatalf("decode refusal %q: %v", body, err)
	}
	if refusal.Error.Code != codeChunkMismatch {
		t.Fatalf("refusal code %q, want %q", refusal.Error.Code, codeChunkMismatch)
	}
	if refusal.Error.Detail.Declared != digest(chunks[0]) || refusal.Error.Detail.Actual != digest(corrupt) {
		t.Fatalf("refusal detail %+v, want the declared and the received digest", refusal.Error.Detail)
	}

	done, err := json.Marshal(completeRequest{SHA256: digest(whole())})
	if err != nil {
		t.Fatalf("marshal completion: %v", err)
	}
	status, body = curlSend(t, base+"/"+up.ID+"/complete", done, append([]string{"-X", "POST"}, jsonHeader...)...)
	if status != http.StatusCreated {
		t.Fatalf("complete: status %d: %s", status, body)
	}
	var finished completeResponse
	if err := json.Unmarshal(body, &finished); err != nil {
		t.Fatalf("decode completion %q: %v", body, err)
	}
	if finished.ID != up.ID || finished.SHA256 != digest(whole()) {
		t.Fatalf("completion answered %+v", finished)
	}
	if !bytes.Equal(x.store.blob(up.ID), whole()) {
		t.Fatal("the stored blob does not match the uploaded file")
	}

	status, body = curlSend(t, base+"/"+up.ID, nil, "-X", "GET")
	if status != http.StatusNotFound {
		t.Fatalf("state of a finished upload: status %d: %s", status, body)
	}
}
