// Package throttle retries a paid call that failed for a reason a wait can
// clear, and it can cap how many of those calls run at once.
//
// A per-minute cap and a transient upstream fault heal on their own. A bad
// request, a rejected key, and a missing model do not, so the caller decides
// which errors are worth waiting on. The wait is full jitter across the upper
// half of each backoff, so a fan-out that was refused together does not retry
// in lockstep. The call's own error is returned untouched. A cancelled context
// ends the wait and still returns that error, never the context's, because the
// caller degrades on what the provider said.
//
// A Gate holds the concurrency cap only while a call runs. The backoff waits
// outside the slot, so a retry does not keep a place another call could use.
package throttle

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

// DefaultAttempts is how many times a call is tried in total, including the
// first, when Config.Attempts is unset.
const DefaultAttempts = 3

// DefaultBackoff is the wait before the second attempt when Config.Backoff is
// unset. Each further wait doubles it, up to DefaultMax.
const DefaultBackoff = 20 * time.Second

// DefaultMax caps one wait when Config.Max is unset, so a large attempt count
// cannot park a call behind an unbounded delay.
const DefaultMax = 90 * time.Second

// Config paces one retry loop. The zero value is usable and means
// DefaultAttempts, DefaultBackoff, and DefaultMax.
type Config struct {
	// Attempts is the total number of tries, including the first. Zero or
	// negative means DefaultAttempts. One disables retrying.
	Attempts int
	// Backoff is the wait before the second attempt. Each later wait doubles
	// it, and no wait exceeds Max. Zero or negative means DefaultBackoff.
	Backoff time.Duration
	// Max caps one wait. Zero or negative means DefaultMax.
	Max time.Duration
}

// Retryable reports whether err is a condition that waiting can clear. A nil
// function retries nothing, so a forgotten classifier cannot spin on a
// permanent failure.
type Retryable func(error) bool

// Call is one attempt. A nil error stops the loop.
type Call func() error

// attempts returns the attempt budget, substituting the default for an unset
// or negative value.
func (c Config) attempts() int {
	if c.Attempts <= 0 {
		return DefaultAttempts
	}
	return c.Attempts
}

// backoff returns the first wait, substituting the default for an unset or
// negative value.
func (c Config) backoff() time.Duration {
	if c.Backoff <= 0 {
		return DefaultBackoff
	}
	return c.Backoff
}

// max returns the wait cap, substituting the default for an unset or negative
// value.
func (c Config) max() time.Duration {
	if c.Max <= 0 {
		return DefaultMax
	}
	return c.Max
}

// Retry runs call until it succeeds, until it fails with an error retryable
// rejects, or until the attempts are spent. It returns call's own error.
// retryable nil treats every error as permanent.
func Retry(ctx context.Context, cfg Config, retryable Retryable, call Call) error {
	return (*Gate)(nil).Retry(ctx, cfg, retryable, call)
}

// Note annotates an error that survived every attempt, so a log can tell a
// cap that was waited out from one that was never retried. An error retryable
// rejects, a nil error, and a nil retryable are returned untouched.
func Note(err error, cfg Config, retryable Retryable) error {
	if err == nil || retryable == nil || !retryable(err) {
		return err
	}
	return fmt.Errorf("still throttled after %d attempts: %w", cfg.attempts(), err)
}

// Gate caps how many paid calls run at once. The zero value and a nil Gate
// impose no cap. Build one with New when a fan-out must share a limit.
type Gate struct {
	sem chan struct{}
}

// New returns a Gate that allows limit calls at once. A limit below one
// returns a gate with no cap.
func New(limit int) *Gate {
	if limit < 1 {
		return &Gate{}
	}
	return &Gate{sem: make(chan struct{}, limit)}
}

// Retry is Retry with this gate's concurrency cap held only for the duration
// of each call. A nil gate imposes no cap.
func (g *Gate) Retry(ctx context.Context, cfg Config, retryable Retryable, call Call) error {
	attempts := cfg.attempts()
	backoff := cfg.backoff()
	ceiling := cfg.max()
	var err error
	for attempt := 1; ; attempt++ {
		if acquireErr := g.acquire(ctx); acquireErr != nil {
			if err != nil {
				return err
			}
			return acquireErr
		}
		err = call()
		g.release()
		if err == nil {
			return nil
		}
		if attempt >= attempts || retryable == nil || !retryable(err) || ctx.Err() != nil {
			return err
		}
		waitFor := jitter(backoff)
		if waitFor > ceiling {
			waitFor = ceiling
		}
		if !wait(ctx, waitFor) {
			return err
		}
		if backoff *= 2; backoff > ceiling {
			backoff = ceiling
		}
	}
}

// acquire takes one concurrency slot, or returns at once when the gate has
// no cap. A cancelled context reports the context error.
func (g *Gate) acquire(ctx context.Context) error {
	if g == nil || g.sem == nil {
		return ctx.Err()
	}
	select {
	case g.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release returns one concurrency slot. A gate with no cap does nothing.
func (g *Gate) release() {
	if g == nil || g.sem == nil {
		return
	}
	<-g.sem
}

// jitter spreads a wait across the upper half of d, so concurrent callers
// that were refused together come back at different moments without any of
// them waiting less than half the intended gap.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// wait sleeps for d and reports whether it completed. A cancelled context
// stops it early and reports false.
func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
