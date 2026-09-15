package mediastore

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireCurl skips a test when curl is absent. These tests exist to
// exercise the handler through a real HTTP client and its own reporting,
// so a library stub would defeat the point.
func requireCurl(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
}

// curlServer starts a real HTTP server in front of s and returns the URL
// prefix a blob id is appended to. The mux supplies the path value the
// handler reads.
func curlServer(t *testing.T, s *Store) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/media/{id}", s)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/media/"
}

// runCurl runs curl and returns its stdout, failing with curl's own
// stderr on a non-zero exit.
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

// TestCurlRangeRequestReturnsPartialContent drives a Range request the
// way a player does and reads the answer out of curl: the status line,
// the byte count curl reports, and the bytes curl wrote to disk.
func TestCurlRangeRequestReturnsPartialContent(t *testing.T) {
	requireCurl(t)
	s := openTestStore(t)
	data := blob(4096)
	blobID := persistPublic(t, s, "application/pdf", data)
	baseURL := curlServer(t, s)
	dir := t.TempDir()
	bodyPath := filepath.Join(dir, "range.bin")
	headerPath := filepath.Join(dir, "range.headers")

	stdout := runCurl(t, "-sS", "-r", "0-99",
		"-D", headerPath, "-o", bodyPath,
		"-w", "%{http_code} %{size_download}", baseURL+blobID)

	if got, want := strings.TrimSpace(stdout), "206 100"; got != want {
		t.Fatalf("curl reported %q, want %q", got, want)
	}
	headers, err := os.ReadFile(headerPath)
	if err != nil {
		t.Fatalf("read headers: %v", err)
	}
	statusLine, _, _ := strings.Cut(string(headers), "\r\n")
	if !strings.Contains(statusLine, "206") {
		t.Fatalf("status line = %q, want a 206", statusLine)
	}
	if !strings.Contains(string(headers), "Content-Range: bytes 0-99/4096") {
		t.Fatalf("headers = %q, want the 0-99 of 4096 range", headers)
	}
	body, err := os.ReadFile(bodyPath)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(body) != 100 {
		t.Fatalf("body = %d bytes, want 100", len(body))
	}
	if !bytes.Equal(body, data[:100]) {
		t.Fatal("range body does not match the first 100 bytes of the blob")
	}

	full := runCurl(t, "-sS", "-o", os.DevNull, "-w", "%{http_code} %{size_download}", baseURL+blobID)
	if got, want := strings.TrimSpace(full), "200 4096"; got != want {
		t.Fatalf("whole-body request reported %q, want %q", got, want)
	}
}

// TestCurlCacheControlPerVisibility reads the headers off a real HEAD
// request for both visibilities, because a caching header a client never
// receives is not a policy.
func TestCurlCacheControlPerVisibility(t *testing.T) {
	requireCurl(t)

	publicStore := openTestStore(t)
	publicID := persistPublic(t, publicStore, "image/png", blob(128))
	publicOutput := runCurl(t, "-sS", "-I", curlServer(t, publicStore)+publicID)
	if !strings.Contains(publicOutput, "HTTP/1.1 200") {
		t.Fatalf("public HEAD = %q, want a 200", publicOutput)
	}
	if !strings.Contains(publicOutput, publicCacheControl) {
		t.Fatalf("public HEAD = %q, want %q", publicOutput, publicCacheControl)
	}

	privateStore := openTestStoreWith(t, func(c *Config) {
		c.Authorize = func(*http.Request, Blob) bool { return true }
	})
	privateID, err := privateStore.Persist(t.Context(), bytes.NewReader(blob(128)), Put{ContentType: "image/png"})
	if err != nil {
		t.Fatalf("persist private: %v", err)
	}
	privateOutput := runCurl(t, "-sS", "-I", curlServer(t, privateStore)+privateID)
	if !strings.Contains(privateOutput, "HTTP/1.1 200") {
		t.Fatalf("private HEAD = %q, want a 200", privateOutput)
	}
	if !strings.Contains(privateOutput, privateCacheControl) {
		t.Fatalf("private HEAD = %q, want %q", privateOutput, privateCacheControl)
	}
	if strings.Contains(privateOutput, "immutable") {
		t.Fatalf("private HEAD = %q, want no immutable directive", privateOutput)
	}
}

// TestCurlPrivateBlobWithoutAuthorizerIsNotFound: a store that cannot
// admit anyone refuses a private blob, and curl sees exactly what an
// unknown id produces.
func TestCurlPrivateBlobWithoutAuthorizerIsNotFound(t *testing.T) {
	requireCurl(t)
	s := openTestStore(t)
	blobID, err := s.Persist(t.Context(), bytes.NewReader(blob(128)), Put{ContentType: "image/png"})
	if err != nil {
		t.Fatalf("persist private: %v", err)
	}
	baseURL := curlServer(t, s)
	bodyPath := filepath.Join(t.TempDir(), "denied.body")

	code := runCurl(t, "-sS", "-o", bodyPath, "-w", "%{http_code}", baseURL+blobID)

	if got := strings.TrimSpace(code); got != "404" {
		t.Fatalf("status = %q, want 404", got)
	}
	body, err := os.ReadFile(bodyPath)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(body), blobID) {
		t.Fatalf("body = %q, want no id", body)
	}
	if strings.Contains(strings.ToLower(string(body)), "private") {
		t.Fatalf("body = %q, want no reason", body)
	}

	headOutput := runCurl(t, "-sS", "-I", baseURL+blobID)
	if !strings.Contains(headOutput, "404") {
		t.Fatalf("private HEAD = %q, want a 404", headOutput)
	}
}
