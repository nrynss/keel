package throttle

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	if !errors.Is(noted, errLimited) || noted.Error() != "still throttled after 3 attempts: limited" {
		t.Fatalf("Note = %v", noted)
	}
	one := Note(errLimited, Config{Attempts: 1}, retryable)
	if !errors.Is(one, errLimited) || one.Error() != "still throttled after 1 attempt: limited" {
		t.Fatalf("Note one = %v", one)
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
		got := jitterAt(nil, d)
		if got < d/2 || got > d {
			t.Fatalf("jitter(%s) = %s", d, got)
		}
	}
	if got := jitterAt(nil, 0); got != 0 {
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

// zero draws nothing, so every jitter returns the bottom of its range and a
// hint gets no spread. A simulation built on it has a single outcome.
func zero(int64) int64 { return 0 }

// at returns a draw that always answers d, for pinning a spread exactly.
func at(d time.Duration) func(int64) int64 {
	return func(int64) int64 { return int64(d) }
}

// overflowedHint is what a caller gets when it multiplies a huge millisecond
// figure into a duration and the product wraps past the top of int64: a
// negative duration that must not be waited.
func overflowedHint() time.Duration {
	ms := time.Millisecond
	return time.Duration(math.MaxInt64) * ms
}

// sleepRecorder is an injected sleep that records each wait and returns at
// once, so a test of the retry loop never really sleeps.
type sleepRecorder struct {
	mu sync.Mutex
	ds []time.Duration
}

func (s *sleepRecorder) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.ds = append(s.ds, d)
	s.mu.Unlock()
	return ctx.Err()
}

func (s *sleepRecorder) waits() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.ds...)
}

func TestTransientAttemptsDefault(t *testing.T) {
	if got := (Config{}).transientAttempts(); got != DefaultTransientAttempts {
		t.Fatalf("zero TransientAttempts = %d, want %d", got, DefaultTransientAttempts)
	}
	if got := (Config{TransientAttempts: -2}).transientAttempts(); got != DefaultTransientAttempts {
		t.Fatalf("negative TransientAttempts = %d, want %d", got, DefaultTransientAttempts)
	}
	if got := (Config{TransientAttempts: 5}).transientAttempts(); got != 5 {
		t.Fatalf("explicit TransientAttempts = %d, want 5", got)
	}
}

// TestRetryTransientBudgetIsSeparate pins that the two classes are counted
// independently, in both directions. A rate-limit refusal is usually free,
// while every transient try is another paid submission, so a large budget for
// one class must not spend the other's.
func TestRetryTransientBudgetIsSeparate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		cfg       Config
		wantCalls int
	}{
		{"transient ignores a large Attempts", errTransient, Config{Attempts: 9}, DefaultTransientAttempts},
		{"limited ignores a large TransientAttempts", errLimited, Config{TransientAttempts: 9}, DefaultAttempts},
		{"explicit TransientAttempts", errTransient, Config{TransientAttempts: 4}, 4},
		{"TransientAttempts of one disables the retry", errTransient, Config{TransientAttempts: 1}, 1},
	} {
		tc.cfg.Transient = func(err error) bool { return errors.Is(err, errTransient) }
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.Backoff = time.Microsecond
			tc.cfg.Max = time.Millisecond
			calls := 0
			err := Retry(context.Background(), tc.cfg, retryable, func() error {
				calls++
				return tc.err
			})
			if !errors.Is(err, tc.err) || calls != tc.wantCalls {
				t.Fatalf("calls = %d, err = %v, want %d calls and %v", calls, err, tc.wantCalls, tc.err)
			}
		})
	}
}

// TestRetryTransientClassifierNilKeepsOneBudget pins the additive path: a
// config that does not tell the classes apart budgets everything as before.
func TestRetryTransientClassifierNilKeepsOneBudget(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), Config{Attempts: 5, Backoff: time.Microsecond}, retryable, func() error {
		calls++
		return errTransient
	})
	if calls != 5 || !errors.Is(err, errTransient) {
		t.Fatalf("calls = %d, err = %v, want 5 calls on the one budget", calls, err)
	}
}

// TestDefaultScheduleFitsTheDefaultWaitBudget is arithmetic, not sampling, so
// it cannot flake. The longest schedule the defaults can produce must fit
// DefaultWaitBudget. Otherwise the last attempt the attempt count promises is
// only reachable when the jitter draws are kind, and a caller who adopts the
// constant can be cut short by it.
func TestDefaultScheduleFitsTheDefaultWaitBudget(t *testing.T) {
	var worst time.Duration
	backoff := DefaultBackoff
	for i := 0; i < DefaultAttempts-1; i++ {
		worst += backoff // jitter never exceeds its base
		if backoff *= 2; backoff > DefaultMax {
			backoff = DefaultMax
		}
	}
	if worst > DefaultWaitBudget {
		t.Fatalf("worst-case default backoff total %s exceeds DefaultWaitBudget %s", worst, DefaultWaitBudget)
	}
	hintWorst := time.Duration(DefaultAttempts-1) * DefaultMax // every wait hint-named at the cap
	if hintWorst > DefaultWaitBudget {
		t.Fatalf("worst-case default hint total %s exceeds DefaultWaitBudget %s", hintWorst, DefaultWaitBudget)
	}
}

// TestRetryWaitBudgetBoundsTheTotal pins the budget's cut: the wait that
// would pass it is never slept, the call ends with the last error still
// matchable, and the wrapper names the attempts actually made.
func TestRetryWaitBudgetBoundsTheTotal(t *testing.T) {
	rec := &sleepRecorder{}
	calls := 0
	cfg := Config{
		Attempts:   9,
		Backoff:    30 * time.Second,
		Max:        45 * time.Second,
		WaitBudget: 50 * time.Second,
		randN:      zero, // waits are exactly 15s, then 22.5s, then 22.5s again
		Sleep:      rec.sleep,
	}
	err := Retry(context.Background(), cfg, retryable, func() error {
		calls++
		return errLimited
	})
	if calls != 3 {
		t.Fatalf("calls = %d, want 3 (two waits of 15s and 22.5s fit, the third wait would pass 50s)", calls)
	}
	if !errors.Is(err, errLimited) {
		t.Fatalf("err = %v, want it to wrap the provider error", err)
	}
	if want := "wait budget 50s spent after 3 attempts"; !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %q, want it to say %q", err, want)
	}
	if w := rec.waits(); len(w) != 2 {
		t.Fatalf("waits = %v, want 2 (the budget cut the third before it slept)", w)
	}
}

// TestRetryWithoutWaitBudgetIsUncapped pins the additive path: a config that
// sets no budget waits as long as its attempt count allows, as before.
func TestRetryWithoutWaitBudgetIsUncapped(t *testing.T) {
	calls := 0
	cfg := Config{Attempts: 5, Backoff: time.Microsecond, Max: time.Millisecond}
	err := Retry(context.Background(), cfg, retryable, func() error {
		calls++
		return errLimited
	})
	if calls != 5 || !errors.Is(err, errLimited) {
		t.Fatalf("calls = %d, err = %v, want 5 calls with no budget in the way", calls, err)
	}
}

func TestRetryHintIsFollowed(t *testing.T) {
	rec := &sleepRecorder{}
	calls := 0
	cfg := Config{
		Backoff:    time.Hour, // would dominate if the hint were ignored
		Max:        time.Minute,
		RetryAfter: func(error) (time.Duration, bool) { return 29970 * time.Millisecond, true },
		randN:      zero,
		Sleep:      rec.sleep,
	}
	err := Retry(context.Background(), cfg, retryable, func() error {
		calls++
		if calls == 1 {
			return errLimited
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("calls = %d, err = %v", calls, err)
	}
	if w := rec.waits(); len(w) != 1 || w[0] != 29970*time.Millisecond {
		t.Fatalf("waits = %v, want [29.97s], the hint with no spread", w)
	}
}

func TestRetryHintCarriesASpread(t *testing.T) {
	rec := &sleepRecorder{}
	calls := 0
	cfg := Config{
		Backoff:    time.Hour,
		Max:        time.Minute,
		RetryAfter: func(error) (time.Duration, bool) { return 30 * time.Second, true },
		randN:      at(500 * time.Millisecond), // the spread draw, under the one-second ceiling
		Sleep:      rec.sleep,
	}
	if err := Retry(context.Background(), cfg, retryable, func() error {
		calls++
		if calls == 1 {
			return errLimited
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if w := rec.waits(); len(w) != 1 || w[0] != 30*time.Second+500*time.Millisecond {
		t.Fatalf("waits = %v, want the hint plus the drawn spread", w)
	}
}

// TestRetryHintIsClamped pins that a hint above Max is clamped before the
// spread is added and again after. A huge or garbled value can therefore
// never overflow into a wait of nearly zero, and a clamped hint still waits
// the full cap.
func TestRetryHintIsClamped(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hint  time.Duration
		randN func(int64) int64
		want  time.Duration
	}{
		{"huge hint clamps to the cap, spread included", time.Duration(1 << 62), at(time.Second), DefaultMax},
		{"just over the cap clamps", DefaultMax + time.Second, at(time.Second), DefaultMax},
		{"small hint carries the spread", 30 * time.Second, at(500 * time.Millisecond), 30*time.Second + 500*time.Millisecond},
		{"small hint with no draw stays exact", 30 * time.Second, zero, 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &sleepRecorder{}
			cfg := Config{
				Backoff: time.Hour,
				Max:     DefaultMax,
				RetryAfter: func(error) (time.Duration, bool) {
					return tc.hint, true
				},
				randN: tc.randN,
				Sleep: rec.sleep,
			}
			calls := 0
			if err := Retry(context.Background(), cfg, retryable, func() error {
				calls++
				if calls == 1 {
					return errLimited
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if w := rec.waits(); len(w) != 1 || w[0] != tc.want {
				t.Fatalf("hint %s: waits = %v, want [%s]", tc.hint, w, tc.want)
			}
		})
	}
}

// TestRetryHintOverflowStillWaitsTheCap pins the corner the clamps exist
// for: a ceiling within a second of the top of int64, and a hint clamped to
// it. Adding the spread wraps the sum negative, and the wait must answer the
// cap, never a negative or near-zero value that would retry at once.
func TestRetryHintOverflowStillWaitsTheCap(t *testing.T) {
	rec := &sleepRecorder{}
	ceiling := time.Duration(math.MaxInt64)
	calls := 0
	cfg := Config{
		Backoff: time.Hour,
		Max:     ceiling,
		RetryAfter: func(error) (time.Duration, bool) {
			return ceiling, true
		},
		randN: at(500 * time.Millisecond), // the spread that overflows the sum
		Sleep: rec.sleep,
	}
	if err := Retry(context.Background(), cfg, retryable, func() error {
		calls++
		if calls == 1 {
			return errLimited
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if w := rec.waits(); len(w) != 1 || w[0] != ceiling {
		t.Fatalf("waits = %v with ceiling %d, want exactly [%d], the cap", w, int64(ceiling), int64(ceiling))
	}
}

// TestRetryGarbledHintFallsBack pins that a hint which is not positive is no
// hint. A caller can overflow a huge value into a negative duration, and
// waiting nothing at all would be a retry hammer.
func TestRetryGarbledHintFallsBack(t *testing.T) {
	for _, hint := range []time.Duration{-5 * time.Second, -1, math.MinInt64, overflowedHint()} {
		rec := &sleepRecorder{}
		cfg := Config{
			Backoff: 20 * time.Second,
			Max:     time.Minute,
			RetryAfter: func(error) (time.Duration, bool) {
				return hint, true
			},
			randN: zero, // the fallback wait is exactly half the backoff
			Sleep: rec.sleep,
		}
		calls := 0
		if err := Retry(context.Background(), cfg, retryable, func() error {
			calls++
			if calls == 1 {
				return errLimited
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if calls != 2 {
			t.Fatalf("hint %d: calls = %d, want 2", hint, calls)
		}
		if w := rec.waits(); len(w) != 1 || w[0] != 10*time.Second {
			t.Fatalf("hint %d: waits = %v, want the backoff fallback [10s]", hint, w)
		}
	}
}

// TestRetryHintOfZeroIsIgnoredSeparatelyFromFalse pins the (0, true) case: a
// zero-length hint is treated as no hint, so the call falls back to the
// backoff instead of retrying at once.
func TestRetryHintOfZeroIsIgnoredSeparatelyFromFalse(t *testing.T) {
	for _, ok := range []bool{true, false} {
		rec := &sleepRecorder{}
		calls := 0
		cfg := Config{
			Backoff: 20 * time.Second,
			Max:     time.Minute,
			RetryAfter: func(error) (time.Duration, bool) {
				return 0, ok
			},
			randN: zero,
			Sleep: rec.sleep,
		}
		if err := Retry(context.Background(), cfg, retryable, func() error {
			calls++
			if calls == 1 {
				return errLimited
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if calls != 2 {
			t.Fatalf("ok = %v: calls = %d, want 2", ok, calls)
		}
	}
}

// TestRetryCancelledWaitMatchesBothErrors pins the contract change: a
// context that ends a wait wraps its error with the last provider error, so
// errors.Is matches either and a cancelled job can be told from a failed one
// without losing what the provider said.
func TestRetryCancelledWaitMatchesBothErrors(t *testing.T) {
	t.Run("cancel during the wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := 0
		err := Retry(ctx, Config{Attempts: 5, Backoff: time.Hour, Sleep: func(ctx context.Context, _ time.Duration) error {
			cancel()
			return ctx.Err()
		}}, retryable, func() error {
			calls++
			return errLimited
		})
		if calls != 1 {
			t.Fatalf("calls = %d, want 1", calls)
		}
		if !errors.Is(err, context.Canceled) || !errors.Is(err, errLimited) {
			t.Fatalf("err = %v, want both context.Canceled and the provider error", err)
		}
	})
	t.Run("deadline ends during the attempt", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		calls := 0
		err := Retry(ctx, Config{Attempts: 5, Backoff: time.Hour}, retryable, func() error {
			calls++
			<-ctx.Done()
			return errLimited
		})
		if calls != 1 {
			t.Fatalf("calls = %d, want 1", calls)
		}
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, errLimited) {
			t.Fatalf("err = %v, want both context.DeadlineExceeded and the provider error", err)
		}
	})
	t.Run("cancelled while the attempt was failing", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		slept := false
		err := Retry(ctx, Config{Attempts: 5, Backoff: time.Hour, Sleep: func(_ context.Context, _ time.Duration) error {
			slept = true
			return nil
		}}, retryable, func() error {
			cancel()
			return errLimited
		})
		if slept {
			t.Fatal("the loop slept after the context had ended")
		}
		if !errors.Is(err, context.Canceled) || !errors.Is(err, errLimited) {
			t.Fatalf("err = %v, want both context.Canceled and the provider error", err)
		}
	})
}

// TestRetryPacerWaitCancelledBeforeTheFirstAttempt pins that a context dead
// on arrival never reaches the call: the pacer refuses it a slot and the
// context error comes back alone, because there is no provider error yet.
func TestRetryPacerWaitCancelledBeforeTheFirstAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := Retry(ctx, Config{PerMinute: 2, Name: "dead-on-arrival", Sleep: func(context.Context, time.Duration) error {
		return nil
	}}, retryable, func() error {
		calls++
		return nil
	})
	if calls != 0 {
		t.Fatalf("calls = %d, want 0", calls)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
