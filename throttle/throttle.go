// Package throttle retries a paid call that failed for a reason a wait can
// clear, paces the starts of those calls under a per-minute cap, and can cap
// how many of them run at once.
//
// A per-minute cap and a transient upstream fault heal on their own. A bad
// request, a rejected key, and a missing model do not, so the caller decides
// which errors are worth waiting on. The wait is full jitter across the upper
// half of each backoff, so a fan-out that was refused together does not retry
// in lockstep. When the provider names how long to wait, the caller supplies
// a func that reads that hint out of the error, and the wait follows the hint
// instead of the backoff. This package never reads a provider's body.
//
// Pacing is the layer before retrying. A Pacer hands out start slots one
// interval apart, so a fan-out of concurrent calls draws on the cap instead
// of spending its attempts on refusals. The cap is usually account-wide, so
// unrelated callers name the window and share one pacer per (name, rate) for
// the life of the process. A pacer cannot see other processes or hosts. It
// also cannot see a submission its caller makes inside a slot without
// waiting, so the retry loop stays as the backstop for refusals pacing let
// through.
//
// Rate-limit refusals and transient faults draw on separate attempt budgets,
// because every transient retry is another paid submission while a refusal is
// usually free. A wait budget bounds the total time one call spends waiting,
// whatever the attempt counts allow.
//
// # Cancelled waits
//
// A context that ends a wait, in the pacer's queue or between attempts, wraps
// its error together with the call's last error, so errors.Is matches either.
// The call's error alone was the older contract, and it recorded a cancelled
// job as a failed one, because job runners tell a cancellation from the
// context error. Permanent errors and exhausted attempts still return the
// provider's error untouched. A cancellation that happens before the first
// attempt started has no provider error to carry and returns the context
// error alone.
//
// A Gate holds the concurrency cap while a call runs. A paced call also holds
// it while its first attempt queues for a rate slot. A slot handed out before
// the cap freed would space the queue and not the starts. The backoff waits
// outside the slot, so a retry does not keep a place another call could use.
package throttle

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

// DefaultAttempts is how many times a call is tried in total, including the
// first, when Config.Attempts is unset. It is the budget for retryable errors
// that Config.Transient does not claim.
const DefaultAttempts = 3

// DefaultTransientAttempts is how many times an error Config.Transient claims
// is tried in total, including the first, when Config.TransientAttempts is
// unset. It is far smaller than DefaultAttempts because a transient fault is
// not a refusal a wait clears, and every further try is another paid
// submission.
const DefaultTransientAttempts = 2

// DefaultBackoff is the wait before the second attempt when Config.Backoff is
// unset. Each further wait doubles it, up to DefaultMax.
const DefaultBackoff = 20 * time.Second

// DefaultMax caps one wait when Config.Max is unset, so a large attempt count
// cannot park a call behind an unbounded delay.
const DefaultMax = 90 * time.Second

// DefaultWaitBudget is the total waiting one call may spend across its
// attempts when the caller sets Config.WaitBudget to it. The default
// schedule's worst case fits with room to spare, so adopting the constant
// never cuts a default-schedule call short. See
// TestDefaultScheduleFitsTheDefaultWaitBudget.
const DefaultWaitBudget = 5 * time.Minute

// Config paces one retry loop. The zero value is usable and behaves exactly
// as it did before pacing, hints, and budgets existed: no pacing, no hint,
// DefaultAttempts for every retryable error, and no total wait cap.
type Config struct {
	// Attempts is the total number of tries, including the first, for
	// retryable errors that Transient does not claim. Zero or negative means
	// DefaultAttempts. One disables retrying.
	Attempts int
	// Transient reports whether err is a transient upstream fault rather
	// than a rate-limit refusal. Claimed errors draw on TransientAttempts.
	// Nil means every retryable error draws on Attempts, which is the
	// behaviour of a config that does not tell the classes apart.
	Transient Retryable
	// TransientAttempts is the total number of tries, including the first,
	// for an error Transient claims. Zero or negative means
	// DefaultTransientAttempts. One disables retrying those errors.
	TransientAttempts int
	// Backoff is the wait before the second attempt. Each later wait doubles
	// it, and no wait exceeds Max. Zero or negative means DefaultBackoff.
	Backoff time.Duration
	// Max caps one wait, whatever it was built from. Zero or negative means
	// DefaultMax.
	Max time.Duration
	// WaitBudget caps the total waiting of one call, across all its waits.
	// A wait that would pass it ends the call with the last error, wrapped
	// so errors.Is still matches. Zero or negative means no total cap, which
	// is the behaviour a config gets when it sets no budget. DefaultWaitBudget
	// is a value the default schedule fits.
	WaitBudget time.Duration
	// RetryAfter reads the wait the provider asked for out of err and
	// reports whether it named one. The wait becomes the hint plus a small
	// spread, clamped to Max. Nil means the backoff is always used. A hint
	// that is not positive is ignored, because an overflowed or garbled
	// value must not shorten a wait.
	RetryAfter func(error) (time.Duration, bool)
	// PerMinute is the per-minute cap the call's starts are paced to. The
	// first start goes at once and later starts are spaced one interval
	// apart, so a fan-out stays under the cap instead of spending attempts
	// on refusals. Zero or negative means no pacing, which is the behaviour
	// of a config that sets no rate.
	PerMinute int
	// Name identifies the account-wide rate window this call joins, such as
	// the model the cap applies to. Calls that name the same window at the
	// same rate share one pacer for the life of the process. Empty means the
	// call is paced on its own. A config that injects Now or Sleep never
	// joins a process-wide pacer, so a test's clock cannot leak into
	// production's.
	Name string
	// Now reads the clock the pacer schedules against. Nil means time.Now.
	Now func() time.Time
	// Sleep waits for d and reports ctx's error if ctx ends first. Nil means
	// a real timer. It is the injectable half of the clock: tests supply one
	// that records d and returns at once.
	Sleep func(ctx context.Context, d time.Duration) error

	// randN draws a uniform integer in [0, n) for jitter and hint spread.
	// Nil means math/rand/v2's global source. Unexported because only this
	// package's tests need a deterministic draw, so a simulation's outcome is
	// a fixed count rather than a sample.
	randN func(n int64) int64
	// registry, when set, is where the pacer is looked up. Unexported
	// because nothing outside this package needs to replace the
	// process-wide one, but this package's tests need to share a pacer on
	// an injected clock and to isolate one from it.
	registry *pacerRegistry
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

// transientAttempts returns the budget for errors Transient claims,
// substituting the default for an unset or negative value.
func (c Config) transientAttempts() int {
	if c.TransientAttempts <= 0 {
		return DefaultTransientAttempts
	}
	return c.TransientAttempts
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
// rejects, or until the attempt budget for that error is spent. A permanent
// error and an exhausted budget return call's own error untouched. A context
// that ends the call between attempts wraps its error with the last one, so
// errors.Is matches both. retryable nil treats every error as permanent.
func Retry(ctx context.Context, cfg Config, retryable Retryable, call Call) error {
	return (*Gate)(nil).Retry(ctx, cfg, retryable, call)
}

// Note annotates an error that survived every attempt, so a log can tell a
// cap that was waited out from one that was never retried. Pass the same
// config the call used. Retry itself still returns the provider error
// untouched, except when a cancelled context or a spent wait budget ended the
// call early. Note is therefore the caller's annotation for the exhaustion
// case. An error retryable rejects, a nil error, and a nil retryable are
// returned untouched. One attempt is singular.
func Note(err error, cfg Config, retryable Retryable) error {
	if err == nil || retryable == nil || !retryable(err) {
		return err
	}
	n := cfg.attempts()
	word := "attempts"
	if n == 1 {
		word = "attempt"
	}
	return fmt.Errorf("still throttled after %d %s: %w", n, word, err)
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
//
// When cfg paces the call, the gate is taken first and the rate slot is
// waited for inside it, once, before the first attempt. A slot is then handed
// out at the moment the caller can really start, instead of arriving while
// the caller still sits in the gate queue and spacing nothing. The price is
// a held place, so a first attempt keeps its concurrency place while it
// queues. The retries inside the loop wait on their own backoff and hint,
// which is the backstop for refusals the pacing could not see coming.
func (g *Gate) Retry(ctx context.Context, cfg Config, retryable Retryable, call Call) error {
	pacer := pacerFor(cfg)
	attempts := cfg.attempts()
	backoff := cfg.backoff()
	ceiling := cfg.max()
	budget := cfg.WaitBudget
	hint := cfg.RetryAfter
	draw := cfg.randN
	rest := cfg.Sleep
	if rest == nil {
		rest = sleep
	}
	var err error
	var waited time.Duration
	var limitedTries, transientTries int
	for attempt := 1; ; attempt++ {
		if acquireErr := g.acquire(ctx); acquireErr != nil {
			if err != nil {
				return interrupted(acquireErr, err)
			}
			return acquireErr
		}
		if attempt == 1 {
			// The rate slot is taken inside the cap, so it is handed
			// out when the caller can really start. A first attempt
			// whose context ends in the queue gives the place back.
			if perr := pacer.Wait(ctx); perr != nil {
				g.release()
				return perr
			}
		}
		err = call()
		g.release()
		if err == nil {
			return nil
		}
		if retryable == nil || !retryable(err) {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return interrupted(cerr, err)
		}
		if cfg.Transient != nil && cfg.Transient(err) {
			transientTries++
			if transientTries >= cfg.transientAttempts() {
				return err
			}
		} else {
			limitedTries++
			if limitedTries >= attempts {
				return err
			}
		}
		waitFor := jitterAt(draw, backoff)
		if d, ok := hintWait(hint, draw, err, ceiling); ok {
			waitFor = d
		}
		if waitFor > ceiling {
			waitFor = ceiling
		}
		if budget > 0 && waited+waitFor > budget {
			return fmt.Errorf("wait budget %s spent after %d attempts: %w", budget, attempt, err)
		}
		if serr := rest(ctx, waitFor); serr != nil {
			return interrupted(serr, err)
		}
		waited += waitFor
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

// interrupted joins the reason a wait ended with the failure that started it,
// keeping both matchable with errors.Is. A caller reads the cancellation from
// the context error and still sees what the provider said.
func interrupted(cause, last error) error {
	return fmt.Errorf("wait interrupted: %w, last error: %w", cause, last)
}

// jitterAt spreads a wait across the upper half of d, so concurrent callers
// that were refused together come back at different moments without any of
// them waiting less than half the intended gap. A nil draw uses the global
// source.
func jitterAt(draw func(n int64) int64, d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	if draw == nil {
		draw = rand.Int64N
	}
	return half + time.Duration(draw(int64(half)+1))
}

// hintWait builds the wait a provider-named hint asks for: the hint plus a
// small spread, so a throttled fan-out does not come back in lockstep and
// re-trip the same window. A hint that is not positive is no hint, because a
// caller can overflow a huge value into a negative duration and a negative
// wait would be a retry hammer. The hint is clamped to the ceiling before the
// spread is added. The sum is guarded too, because a ceiling within a second
// of the top of int64 can wrap the addition negative, and a sum past either
// bound answers the ceiling.
func hintWait(hint func(error) (time.Duration, bool), draw func(int64) int64, err error, ceiling time.Duration) (time.Duration, bool) {
	if hint == nil {
		return 0, false
	}
	d, ok := hint(err)
	if !ok || d <= 0 {
		return 0, false
	}
	if d > ceiling {
		d = ceiling
	}
	if draw == nil {
		draw = rand.Int64N
	}
	d += time.Duration(draw(int64(time.Second) + 1))
	if d <= 0 || d > ceiling {
		d = ceiling
	}
	return d, true
}

// sleep sleeps for d and reports ctx's error when ctx ends first.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
