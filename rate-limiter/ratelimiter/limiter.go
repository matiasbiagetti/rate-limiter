// Package ratelimiter implements an in-memory, token-bucket rate limiter.
//
// The package is deliberately transport-agnostic: it knows nothing about HTTP.
// The HTTP middleware lives in a separate package that depends on this one,
// keeping the core pure and independently testable. See DESIGN.md for the
// rationale behind the algorithm, concurrency, and eviction choices.
package ratelimiter

import (
	"sync"
	"time"
)

// Decision is the outcome of an Allow check.
//
// Note there is no error return: in-memory limiting cannot fail at runtime, so
// errors are "defined out of existence" (APOSD). Invalid configuration is a
// caller concern, validated via Rate.Validate at the boundary. This keeps the
// hot path branch-free of error handling.
type Decision struct {
	Allowed    bool          // whether the request may proceed
	Remaining  int           // whole tokens left in the bucket after this call
	RetryAfter time.Duration // when denied, how long until one token is available
}

// Limiter is a concurrency-safe registry of per-key token buckets.
//
// Concurrency: a single mutex guards the whole bucket map and the per-bucket
// read-modify-write. Holding it across the entire bucket.allow call is what
// makes "refill, check, consume" atomic — the operation that, if interleaved,
// would let two requests double-spend the same token. Every Allow mutates, so a
// plain sync.Mutex is used rather than an RWMutex (there are no pure readers).
// Sharding the map by key hash is the documented scaling path if benchmarks
// show contention; because the lock is encapsulated here, that would not change
// the public API.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	clock   Clock
	idleTTL time.Duration

	stop   chan struct{} // closed by Close to stop the janitor
	closed bool
}

// New constructs a Limiter and starts the eviction janitor if Options enables
// it (SweepInterval > 0). It cannot fail, so it returns no error.
func New(opts Options) *Limiter {
	opts = opts.withDefaults()
	l := &Limiter{
		buckets: make(map[string]*tokenBucket),
		clock:   opts.Clock,
		idleTTL: opts.IdleTTL,
		stop:    make(chan struct{}),
	}
	if opts.SweepInterval > 0 {
		go l.runJanitor(opts.SweepInterval)
	}
	return l
}

// Allow reports whether a request identified by key may proceed under rate.
//
// Passing rate per call (instead of fixing it on the Limiter) enables flexible
// per-request limit configurations: the caller decides which policy applies and
// the Limiter enforces it for that key. If the policy for an existing key
// changes, the new rate is adopted and any surplus tokens are clamped to the
// new capacity.
func (l *Limiter) Allow(key string, rate Rate) Decision {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clock.Now()
	b, ok := l.buckets[key]
	switch {
	case !ok:
		b = newBucket(rate, now)
		l.buckets[key] = b
	case b.rate != rate:
		b.rate = rate
		if capacity := float64(rate.Capacity); b.tokens > capacity {
			b.tokens = capacity
		}
	}
	return b.allow(now)
}

// Len returns the number of keys currently tracked. Useful as a memory metric
// and for asserting eviction behaviour in tests.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// Close stops the background janitor and releases resources. It is safe to call
// more than once; later calls are no-ops.
func (l *Limiter) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	close(l.stop)
	return nil
}

// sweep evicts idle buckets using the current time. It takes the lock, so it
// must not be called while already holding it.
func (l *Limiter) sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.evictIdle(l.clock.Now())
}
