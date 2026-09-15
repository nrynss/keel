package gate

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// TestBucketDoesNotMintTokensOnABackwardsClock covers the branch that
// exists because a container's clock can step backwards after an NTP
// correction: elapsed <= 0 must refill nothing, and must not move the
// bucket's reference point either.
func TestBucketDoesNotMintTokensOnABackwardsClock(t *testing.T) {
	start := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	b := newBucket(Limit{Burst: 2, Every: time.Minute}, start)
	b.take()
	b.take()
	if b.available() {
		t.Fatal("bucket still has tokens after spending its whole burst")
	}

	b.refill(start.Add(-time.Hour))
	if b.available() {
		t.Fatal("a backwards clock minted a token")
	}
	if !b.last.Equal(start) {
		t.Fatalf("a backwards clock moved the bucket's reference point to %v, want %v", b.last, start)
	}
	b.refill(start)
	if b.available() {
		t.Fatal("a clock that did not advance minted a token")
	}

	// Real forward time still works, and is measured from the original
	// reference point rather than from the backwards one.
	b.refill(start.Add(time.Minute))
	if !b.available() {
		t.Fatal("a minute of real time did not refill a token")
	}
	if b.full() {
		t.Fatal("one minute refilled the whole burst of 2")
	}
	b.refill(start.Add(time.Hour))
	if !b.full() {
		t.Fatal("an hour did not refill the bucket to its burst")
	}
}

func TestBucketRetryAfterIsNeverZeroForAnEmptyBucket(t *testing.T) {
	start := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	// A refill interval far below a second: the wait rounds to zero and
	// must be floored, or a client is told to retry immediately into
	// another refusal.
	b := newBucket(Limit{Burst: 1, Every: time.Millisecond}, start)
	b.take()
	if got := b.retryAfter(); got != time.Second {
		t.Fatalf("retryAfter on a sub-second bucket = %v, want the 1s floor", got)
	}
	// A bucket that can pay owes no wait at all.
	b.refill(start.Add(time.Second))
	if got := b.retryAfter(); got != 0 {
		t.Fatalf("retryAfter on a full bucket = %v, want 0", got)
	}
}

// TestRetryAfterHeaderFloorsAtOneSecond is the same floor observed
// through the wire, which is where it actually matters.
func TestRetryAfterHeaderFloorsAtOneSecond(t *testing.T) {
	c := newClock()
	g := mustGate(t, Config{Now: c.now})
	rule := Rule{
		Name:      "fast",
		PerClient: Limit{Burst: 1, Every: 10 * time.Millisecond},
		Global:    Limit{Burst: 1000, Every: time.Second},
	}
	h := mustProtect(t, g, rule, &counted{})
	h.ServeHTTP(httptest.NewRecorder(), request("203.0.113.1:1", nil))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, request("203.0.113.1:1", nil))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	seconds, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if err != nil || seconds < 1 {
		t.Fatalf("Retry-After = %q (err %v), want at least 1", w.Header().Get("Retry-After"), err)
	}
}

// TestGlobalRefusalCarriesItsOwnRetryAfter covers the branch where the
// per-client bucket can pay but the route's global budget cannot.
func TestGlobalRefusalCarriesItsOwnRetryAfter(t *testing.T) {
	c := newClock()
	g := mustGate(t, Config{Now: c.now})
	rule := Rule{
		Name:      "generate",
		PerClient: Limit{Burst: 10, Every: time.Second},
		Global:    Limit{Burst: 1, Every: 30 * time.Minute},
	}
	h := mustProtect(t, g, rule, &counted{})
	h.ServeHTTP(httptest.NewRecorder(), request("203.0.113.1:1", nil))
	w := httptest.NewRecorder()
	// A different client, so the per-client bucket is untouched and only
	// the global one can refuse.
	h.ServeHTTP(w, request("203.0.113.2:1", nil))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	seconds, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if err != nil || seconds != int((30*time.Minute).Seconds()) {
		t.Fatalf("Retry-After = %q (err %v), want %d", w.Header().Get("Retry-After"), err, int((30 * time.Minute).Seconds()))
	}
}
