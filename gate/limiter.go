package gate

import "time"

// bucket is one token bucket. Tokens are fractional so a refill
// interval need not divide the elapsed time evenly. The alternative,
// integer refills, silently rounds a slow drip down to nothing.
//
// A bucket is only advanced when it is touched (refill), so an idle
// bucket costs nothing until the next request for its key. Its stored
// token count is therefore a lower bound between touches, which is what
// makes the "evict a full bucket first" rule in evictLocked safe: a
// bucket that already reads full cannot become less than full while
// nobody is drawing on it.
type bucket struct {
	limit  Limit
	tokens float64
	last   time.Time
}

// newBucket returns a full bucket for limit, as of now.
func newBucket(limit Limit, now time.Time) *bucket {
	return &bucket{limit: limit, tokens: float64(limit.Burst), last: now}
}

// refill advances the bucket to now, capped at its burst.
func (b *bucket) refill(now time.Time) {
	elapsed := now.Sub(b.last)
	if elapsed <= 0 {
		// A clock that did not advance (or went backwards) must not
		// mint tokens. Hold last where it is so the next real tick is
		// measured from the same point.
		return
	}
	b.last = now
	b.tokens += float64(elapsed) / float64(b.limit.Every)
	if b.tokens > float64(b.limit.Burst) {
		b.tokens = float64(b.limit.Burst)
	}
}

// available reports whether a whole token can be taken.
func (b *bucket) available() bool {
	return b.tokens >= 1
}

// full reports whether the bucket is at capacity, which means it carries
// no debt at all.
func (b *bucket) full() bool {
	return b.tokens >= float64(b.limit.Burst)
}

// take spends one token. The caller has already checked available.
func (b *bucket) take() {
	b.tokens--
}

// retryAfter is how long until the bucket holds a whole token again. It
// is never zero for an empty bucket: a Retry-After of 0 invites an
// immediate retry that would be refused again.
func (b *bucket) retryAfter() time.Duration {
	missing := 1 - b.tokens
	if missing <= 0 {
		return 0
	}
	wait := time.Duration(missing * float64(b.limit.Every))
	if wait < time.Second {
		return time.Second
	}
	return wait
}
