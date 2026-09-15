package wire

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ruleDetail names the rule that refused a request.
type ruleDetail struct {
	Rule string `json:"rule"`
}

// retryDetail reports the wait in whole seconds before a client retries.
type retryDetail struct {
	RetryAfterSeconds int `json:"retry_after_seconds"`
}

// fractionalRetryDetail reports a wait in seconds with a fraction, as a
// producer computes it from a refill time.
type fractionalRetryDetail struct {
	RetryAfterSeconds float64 `json:"retry_after_seconds"`
}

// errWriteFailed is the write failure that failingWriter reports.
var errWriteFailed = errors.New("disk full")

// failingWriter is an http.ResponseWriter whose body write always fails.
type failingWriter struct {
	header http.Header
	status int
}

func newFailingWriter() *failingWriter {
	return &failingWriter{header: make(http.Header)}
}

func (w *failingWriter) Header() http.Header { return w.header }

func (w *failingWriter) WriteHeader(status int) { w.status = status }

func (w *failingWriter) Write([]byte) (int, error) { return 0, errWriteFailed }

func TestWriteErrorGolden(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		code       string
		message    string
		detail     any
		retryAfter string
		golden     string
	}{
		{
			name:    "refusal with detail",
			status:  http.StatusForbidden,
			code:    "forbidden",
			message: "This request needs a passcode.",
			detail:  ruleDetail{Rule: "upload"},
			golden:  "error-forbidden.json",
		},
		{
			name:    "refusal without detail",
			status:  http.StatusNotFound,
			code:    "not_found",
			message: "No such upload exists.",
			golden:  "error-not-found.json",
		},
		{
			name:       "rate limited",
			status:     http.StatusTooManyRequests,
			code:       "rate_limited",
			message:    "Too many requests.",
			detail:     retryDetail{RetryAfterSeconds: 12},
			retryAfter: "12",
			golden:     "error-rate-limited.json",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			err := WriteError(rec, tt.status, tt.code, tt.message, tt.detail)
			if err != nil {
				t.Fatalf("WriteError returned an error: %v", err)
			}
			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d", rec.Code, tt.status)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want %q", got, "application/json")
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want %q", got, "no-store")
			}
			if got := rec.Header().Get("Retry-After"); got != tt.retryAfter {
				t.Errorf("Retry-After = %q, want %q", got, tt.retryAfter)
			}
			want, err := os.ReadFile(filepath.Join("..", "testdata", "wire", tt.golden))
			if err != nil {
				t.Fatalf("read golden file: %v", err)
			}
			if got := rec.Body.Bytes(); !bytes.Equal(got, want) {
				t.Fatalf("body does not match %s\n got: %q\nwant: %q", tt.golden, got, want)
			}
		})
	}
}

func TestWriteErrorRetryAfter(t *testing.T) {
	tests := []struct {
		name   string
		detail any
		want   string
	}{
		{"no detail", nil, "1"},
		{"zero hint", retryDetail{}, "1"},
		{"fraction below one second", fractionalRetryDetail{RetryAfterSeconds: 0.4}, "1"},
		{"fraction rounds up", fractionalRetryDetail{RetryAfterSeconds: 12.5}, "13"},
		{"fraction below one half rounds up", fractionalRetryDetail{RetryAfterSeconds: 12.4}, "13"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			err := WriteError(rec, http.StatusTooManyRequests, "rate_limited", "Too many requests.", tt.detail)
			if err != nil {
				t.Fatalf("WriteError returned an error: %v", err)
			}
			if got := rec.Header().Get("Retry-After"); got != tt.want {
				t.Errorf("Retry-After = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWriteErrorDetailFallback(t *testing.T) {
	tests := []struct {
		name    string
		detail  any
		wantErr bool
	}{
		{"inencodable detail drops the member and reports it", make(chan int), true},
		{"detail that encodes to null counts as absent", (*ruleDetail)(nil), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			err := WriteError(rec, http.StatusConflict, "conflict", "The upload changed meanwhile.", tt.detail)
			if (err != nil) != tt.wantErr {
				t.Fatalf("WriteError returned error %v, want error presence %v", err, tt.wantErr)
			}
			want := []byte("{\"error\":{\"code\":\"conflict\",\"message\":\"The upload changed meanwhile.\"}}\n")
			if got := rec.Body.Bytes(); !bytes.Equal(got, want) {
				t.Fatalf("body = %q, want %q", got, want)
			}
		})
	}
}

func TestWriteErrorFailedWrite(t *testing.T) {
	tests := []struct {
		name       string
		detail     any
		wantJoined bool
	}{
		{"failed write comes back as the error", ruleDetail{Rule: "upload"}, false},
		{"failed write joins an inencodable detail error", make(chan int), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newFailingWriter()
			err := WriteError(w, http.StatusTooManyRequests, "rate_limited", "Too many requests.", tt.detail)
			if !errors.Is(err, errWriteFailed) {
				t.Fatalf("WriteError returned %v, want an error wrapping the write failure", err)
			}
			if w.status != http.StatusTooManyRequests {
				t.Errorf("status = %d, want %d", w.status, http.StatusTooManyRequests)
			}
			if tt.wantJoined && !strings.Contains(err.Error(), "encode error detail") {
				t.Errorf("error %q does not carry the failed detail encoding", err)
			}
		})
	}
}
