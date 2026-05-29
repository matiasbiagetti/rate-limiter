package ratelimiter

import "time"

// Clock is the limiter's only source of time. Injecting it (instead of calling
// time.Now directly) makes refill and eviction behaviour deterministically
// testable without sleeping. Production uses realClock; tests use ManualClock.
type Clock interface {
	Now() time.Time
}

// realClock is the production Clock, backed by the monotonic system clock.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// ManualClock is a test Clock whose time only advances when explicitly told.
// It is exported so package tests (and the demo) can drive time precisely.
type ManualClock struct {
	t time.Time
}

// NewManualClock returns a ManualClock anchored at the given instant.
func NewManualClock(start time.Time) *ManualClock { return &ManualClock{t: start} }

// Now returns the clock's current (manually controlled) time.
func (c *ManualClock) Now() time.Time { return c.t }

// Advance moves the clock forward by d.
func (c *ManualClock) Advance(d time.Duration) { c.t = c.t.Add(d) }
