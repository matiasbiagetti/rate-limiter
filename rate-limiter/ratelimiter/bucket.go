package ratelimiter

import "time"

// tokenBucket is the per-key state of the token-bucket algorithm. Tokens are
// refilled lazily on access from the time elapsed since lastSeen, so there is
// no background goroutine or timer per key — only arithmetic when a key is hit.
//
// tokenBucket is not safe for concurrent use; the owning Limiter serialises
// access to it under its mutex.
type tokenBucket struct {
	tokens   float64   // current whole/fractional tokens available
	lastSeen time.Time // last time tokens were refilled (also drives eviction)
	rate     Rate      // the limit this bucket is currently configured for
}

// newBucket returns a bucket that starts full, so a fresh client may burst up
// to Capacity immediately, configured for rate as of now.
func newBucket(rate Rate, now time.Time) *tokenBucket {
	return &tokenBucket{
		tokens:   float64(rate.Capacity),
		lastSeen: now,
		rate:     rate,
	}
}

// refill credits tokens accrued between lastSeen and now, capped at capacity,
// and advances lastSeen. If no time has elapsed (or the clock appears to move
// backwards) it is a no-op, leaving lastSeen untouched so repeated calls at the
// same instant cannot double-count.
func (b *tokenBucket) refill(now time.Time) {
	elapsed := now.Sub(b.lastSeen)
	if elapsed <= 0 {
		return
	}
	// Tokens accrue at Capacity per Interval. Multiply before dividing to keep
	// full float precision; elapsed and Interval are both nanosecond counts.
	b.tokens += float64(elapsed) * float64(b.rate.Capacity) / float64(b.rate.Interval)
	if capacity := float64(b.rate.Capacity); b.tokens > capacity {
		b.tokens = capacity
	}
	b.lastSeen = now
}

// allow refills as of now, consumes one token if at least one is available,
// and returns the resulting Decision. When denied, RetryAfter is the time until
// the next whole token accrues.
func (b *tokenBucket) allow(now time.Time) Decision {
	b.refill(now)

	if b.tokens >= 1 {
		b.tokens--
		return Decision{Allowed: true, Remaining: int(b.tokens)}
	}

	// Time for the missing fraction of a token to refill: needed * Interval /
	// Capacity (multiply before divide, as in refill).
	needed := 1 - b.tokens
	retryAfter := time.Duration(needed * float64(b.rate.Interval) / float64(b.rate.Capacity))
	return Decision{Allowed: false, Remaining: 0, RetryAfter: retryAfter}
}
