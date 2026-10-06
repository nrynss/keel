package outbox

import (
	"testing"
	"time"
)

// TestZeroConfigResolvesDefaults: the zero Config is usable, and every
// accessor resolves to its documented default.
func TestZeroConfigResolvesDefaults(t *testing.T) {
	var cfg Config
	if got := cfg.batch(); got != DefaultBatchSize {
		t.Errorf("batch() = %d, want %d", got, DefaultBatchSize)
	}
	if got := cfg.interval(); got != DefaultInterval {
		t.Errorf("interval() = %v, want %v", got, DefaultInterval)
	}
	if got := cfg.attempts(); got != DefaultMaxAttempts {
		t.Errorf("attempts() = %d, want %d", got, DefaultMaxAttempts)
	}
	if got := cfg.wait(); got != DefaultRetryWait {
		t.Errorf("wait() = %v, want %v", got, DefaultRetryWait)
	}
	if got := cfg.maxWait(); got != DefaultRetryMax {
		t.Errorf("maxWait() = %v, want %v", got, DefaultRetryMax)
	}
}

// TestMaxWaitNeverBelowFirstWait: a cap below the first retry wait reads as
// the first wait, so the doubling curve starts inside its own range.
func TestMaxWaitNeverBelowFirstWait(t *testing.T) {
	cfg := Config{RetryWait: 30 * time.Second, RetryMax: 5 * time.Second}
	if got := cfg.maxWait(); got != 30*time.Second {
		t.Errorf("maxWait() = %v, want the first wait", got)
	}
}

// TestNextWaitsDoublesUpToTheCap: each failed pass doubles the base, and
// the cap holds the curve once it is reached.
func TestNextWaitsDoublesUpToTheCap(t *testing.T) {
	wait, retry := nextWaits(time.Second, time.Minute, 5*time.Second)
	if wait != 5*time.Second {
		t.Errorf("wait = %v, want the first retry wait", wait)
	}
	if retry != 10*time.Second {
		t.Errorf("retry = %v, want the doubled base", retry)
	}

	wait, retry = nextWaits(time.Second, time.Minute, 64*time.Second)
	if wait != 64*time.Second {
		t.Errorf("wait = %v, want the doubled base", wait)
	}
	if retry != time.Minute {
		t.Errorf("retry = %v, want the cap", retry)
	}

	wait, retry = nextWaits(time.Second, time.Minute, time.Minute)
	if wait != time.Minute || retry != time.Minute {
		t.Errorf("wait, retry = %v, %v, want the cap on both", wait, retry)
	}
}

// TestNextWaitsNeverUndercutsTheInterval: a retry wait below the pass
// interval reads as the interval, so a failing sink is never polled faster
// than a healthy one.
func TestNextWaitsNeverUndercutsTheInterval(t *testing.T) {
	wait, retry := nextWaits(10*time.Second, time.Minute, 5*time.Second)
	if wait != 10*time.Second {
		t.Errorf("wait = %v, want the interval", wait)
	}
	if retry != 10*time.Second {
		t.Errorf("retry = %v, want the doubled base", retry)
	}
}
