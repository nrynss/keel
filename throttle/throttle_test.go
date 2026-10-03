package throttle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	errLimited   = errors.New("limited")
	errTransient = errors.New("transient")
	errPermanent = errors.New("permanent")
)

func retryable(err error) bool {
	return errors.Is(err, errLimited) || errors.Is(err, errTransient)
}

// fast is every retry test's pacing. The decision under test is which errors
// wait at all, not how long. Backoff is a real timer.
var fast = Config{Backoff: time.Microsecond, Max: time.Millisecond}

func TestConfigDefaults(t *testing.T) {
	got := Config{}
	if got.attempts() != DefaultAttempts || got.backoff() != DefaultBackoff || got.max() != DefaultMax {
		t.Fatalf("zero config = %d %s %s", got.attempts(), got.backoff(), got.max())
	}
	neg := Config{Attempts: -3, Backoff: -time.Second, Max: -time.Second}
	if neg.attempts() != DefaultAttempts || neg.backoff() != DefaultBackoff || neg.max() != DefaultMax {
		t.Fatalf("negative config was not defaulted")
	}
	explicit := Config{Attempts: 7, Backoff: time.Minute, Max: 2 * time.Minute}
	if explicit.attempts() != 7 || explicit.backoff() != time.Minute || explicit.max() != 2*time.Minute {
		t.Fatalf("explicit config was rewritten")
	}
}

func TestRetryWaitsOutACap(t *testing.T) {
	cases := []struct {
		name      string
		failures  int
		err       error
		wantCalls int
		wantErr   error
	}{
		{name: "clears on the second try", failures: 1, err: errLimited, wantCalls: 2},
		{name: "clears on the last try", failures: 2, err: errLimited, wantCalls: 3},
		{name: "never clears", failures: 9, err: errLimited, wantCalls: 3, wantErr: errLimited},
		{name: "transient is waited on", failures: 9, err: errTransient, wantCalls: 3, wantErr: errTransient},
		{name: "wrapped sentinel still counts", failures: 1, err: fmt.Errorf("page 3: %w", errLimited), wantCalls: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := Retry(context.Background(), fast, retryable, func() error {
				calls++
				if calls <= tc.failures {
					return tc.err
				}
				return nil
			})
			if calls != tc.wantCalls {
				t.Fatalf("calls = %d, want %d", calls, tc.wantCalls)
			}
			if tc.wantErr == nil && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestRetrySurfacesPermanentErrorsAtOnce(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), fast, retryable, func() error {
		calls++
		return errPermanent
	})
	if calls != 1 || !errors.Is(err, errPermanent) {
		t.Fatalf("calls = %d, err = %v", calls, err)
	}
}

func TestRetryNilClassifierDoesNotRetry(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), fast, nil, func() error {
		calls++
		return errLimited
	})
	if calls != 1 || !errors.Is(err, errLimited) {
		t.Fatalf("calls = %d, err = %v", calls, err)
	}
}

func TestRetryAttemptsOfOneNeverRetries(t *testing.T) {
	calls := 0
	cfg := Config{Attempts: 1, Backoff: time.Microsecond}
	err := Retry(context.Background(), cfg, retryable, func() error {
		calls++
		return errLimited
	})
	if calls != 1 || !errors.Is(err, errLimited) {
		t.Fatalf("calls = %d, err = %v", calls, err)
	}
}

func TestRetryCancelledContextStopsTheWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := Retry(ctx, Config{Attempts: 5, Backoff: time.Hour}, retryable, func() error {
		calls++
		cancel()
		return errLimited
	})
	if calls != 1 || !errors.Is(err, errLimited) {
		t.Fatalf("calls = %d, err = %v", calls, err)
	}
}

func TestRetryCancelledBeforeCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := Retry(ctx, fast, retryable, func() error {
		calls++
		return nil
	})
	if calls != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("calls = %d, err = %v", calls, err)
	}
}

func TestNote(t *testing.T) {
	noted := Note(errLimited, Config{Attempts: 3}, retryable)
	if !errors.Is(noted, errLimited) || !strings.Contains(noted.Error(), "still throttled after 3 attempts") {
		t.Fatalf("Note = %v", noted)
	}
	plain := errors.New("something else")
	if got := Note(plain, Config{}, retryable); got != plain {
		t.Fatalf("Note(%v) = %v", plain, got)
	}
	if got := Note(errLimited, Config{}, nil); got != errLimited {
		t.Fatalf("nil classifier Note = %v", got)
	}
}

func TestJitterStaysInTheUpperHalf(t *testing.T) {
	const d = 20 * time.Second
	for i := 0; i < 200; i++ {
		got := jitter(d)
		if got < d/2 || got > d {
			t.Fatalf("jitter(%s) = %s", d, got)
		}
	}
	if got := jitter(0); got != 0 {
		t.Fatalf("jitter(0) = %s", got)
	}
}

func TestGateCapsInFlightCalls(t *testing.T) {
	const limit = 2
	gate := New(limit)
	var current atomic.Int32
	var peak atomic.Int32
	var wg sync.WaitGroup
	release := make(chan struct{})
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := gate.Retry(context.Background(), Config{Attempts: 1}, nil, func() error {
				n := current.Add(1)
				for {
					old := peak.Load()
					if n <= old || peak.CompareAndSwap(old, n) {
						break
					}
				}
				<-release
				current.Add(-1)
				return nil
			})
			if err != nil {
				t.Errorf("call: %v", err)
			}
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for peak.Load() < limit && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if got := peak.Load(); got != limit {
		t.Fatalf("peak in flight = %d, want %d", got, limit)
	}
	close(release)
	wg.Wait()
}

func TestNewUnlimited(t *testing.T) {
	if g := New(0); g.sem != nil {
		t.Fatal("New(0) imposed a cap")
	}
	if g := New(-1); g.sem != nil {
		t.Fatal("New(-1) imposed a cap")
	}
}
