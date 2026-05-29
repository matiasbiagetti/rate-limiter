package ratelimiter

import (
	"errors"
	"time"
)

// Rate describes a token-bucket limit: a bucket holds up to Capacity
// tokens and is refilled to full over one Interval (i.e. the steady-state
// allowance is Capacity requests per Interval, with bursts up to Capacity).
//
// Rate is a small immutable value object. It is passed to Limiter.Allow on
// every call rather than fixed at construction time, which is the seam that
// lets a single Limiter serve flexible, per-request limit configurations
// (per-route, per-tier, per-client-plan).
type Rate struct {
	Capacity int
	Interval time.Duration
}

// ErrInvalidRate is returned by Validate for a non-positive capacity or interval.
var ErrInvalidRate = errors.New("ratelimiter: rate must have positive capacity and interval")

// Validate reports whether the rate is usable. Invalid configuration is an
// error surfaced at the boundary (construction / config load), never during
// the hot Allow path.
func (r Rate) Validate() error {
	if r.Capacity <= 0 || r.Interval <= 0 {
		return ErrInvalidRate
	}
	return nil
}
