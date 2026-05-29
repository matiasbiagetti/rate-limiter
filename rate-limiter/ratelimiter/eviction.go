package ratelimiter

import "time"

// Eviction bounds the limiter's memory. Each distinct key creates a bucket, so
// a flood of unique keys (e.g. spoofed client IPs) is otherwise an unbounded
// memory-growth / denial-of-service vector. A background janitor periodically
// removes buckets that are safe to drop.

// runJanitor loops on a ticker, sweeping idle buckets until stop is closed.
func (l *Limiter) runJanitor(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			l.sweep()
		}
	}
}

// evictIdle removes buckets that are safe to drop. The caller must hold l.mu.
//
// A bucket is only evicted once it has been idle for at least its own Interval,
// meaning it has fully refilled to capacity. Eviction is then lossless:
// recreating the bucket on the next request yields an identical full bucket, so
// no client can evade its limit by waiting out the janitor. IdleTTL adds extra
// grace on top of that correctness floor.
func (l *Limiter) evictIdle(now time.Time) {
	for key, b := range l.buckets {
		idle := now.Sub(b.lastSeen)
		if idle >= b.rate.Interval && idle >= l.idleTTL {
			delete(l.buckets, key)
		}
	}
}
