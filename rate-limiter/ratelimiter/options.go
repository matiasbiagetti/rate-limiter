package ratelimiter

import "time"

// Options configures a Limiter. The zero value is usable: it yields a limiter
// with the system clock and no background eviction.
type Options struct {
	// Clock is the time source. Defaults to the system clock when nil.
	Clock Clock

	// SweepInterval is how often the background janitor scans for idle buckets.
	// Eviction is opt-in: a value <= 0 disables the janitor entirely. Starting
	// a background goroutine is a deliberate choice left to the caller rather
	// than a hidden default.
	SweepInterval time.Duration

	// IdleTTL is an extra grace period a bucket must be idle, on top of the
	// correctness floor (a bucket is only ever evicted once it has fully
	// refilled). Bounds memory under floods of unique keys.
	IdleTTL time.Duration
}

// withDefaults returns a copy of opts with unset fields filled in. Only the
// clock has a behavioural default; eviction stays opt-in.
func (o Options) withDefaults() Options {
	if o.Clock == nil {
		o.Clock = realClock{}
	}
	return o
}
