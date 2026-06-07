package ratelimiter

import "time"

// Clock is the limiter's single source of time, used as a dependency-injection
// seam: the Limiter never calls time.Now directly; it reads the time through
// whatever Clock it was given. Production wires realClock; tests wire ManualClock
// and advance it by hand, which makes time-dependent behaviour (token refill and
// eviction) deterministic and fast to test, with no time.Sleep.
type Clock interface {
	Now() time.Time
}

// realClock is the production Clock: a stateless wrapper over the system's
// monotonic clock. A value receiver suffices because it holds no state.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// ManualClock is a test Clock whose time only moves when Advance is called, so a
// test can simulate "30s elapsed" instantly. It is exported so package tests (and
// the demo) can drive time precisely.
//
// Its methods use a pointer receiver because Advance mutates the stored instant;
// that is why NewManualClock returns *ManualClock (only the pointer satisfies Clock).
type ManualClock struct {
	t time.Time
}

// NewManualClock returns a ManualClock anchored at the given instant.
func NewManualClock(start time.Time) *ManualClock { return &ManualClock{t: start} }

// Now returns the clock's current (manually controlled) time.
func (c *ManualClock) Now() time.Time { return c.t }

// Advance moves the clock forward by d, simulating elapsed time in tests.
func (c *ManualClock) Advance(d time.Duration) { c.t = c.t.Add(d) }
