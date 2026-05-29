// Package middleware adapts the transport-agnostic ratelimiter core to
// net/http. It is the only package that imports both net/http and ratelimiter,
// keeping the dependency direction one-way (middleware -> ratelimiter).
package middleware

import (
	"errors"
	"math"
	"net"
	"net/http"
	"strconv"

	"github.com/biagettimati/rate-limiter/ratelimiter"
)

// KeyFunc extracts the rate-limit key from a request — typically the client IP
// or an API token. Injecting it means the limiting dimension is not hardcoded.
type KeyFunc func(*http.Request) string

// PolicyFunc selects which Rate applies to a request. This is the seam for
// flexible limit configurations: return different rates per route, per client
// tier, or per plan. Use FixedRate for a single global limit.
type PolicyFunc func(*http.Request) ratelimiter.Rate

// Config wires the middleware to a limiter and the two policy seams.
type Config struct {
	// Limiter enforces the limits. Required.
	Limiter *ratelimiter.Limiter
	// Policy chooses the Rate per request. Required.
	Policy PolicyFunc
	// Key extracts the limiting key. Optional; defaults to ClientIP.
	Key KeyFunc
	// OnDenied writes the response for a rejected request. It is optional and
	// defaults to a plain-text 429. When it runs, the X-RateLimit-* and
	// Retry-After headers are already set on the ResponseWriter, so a custom
	// handler can read them (e.g. to echo Retry-After into a JSON body) and
	// only needs to choose a status/body.
	OnDenied http.Handler
}

// Configuration errors. These are programmer errors caught at startup, which is
// why they are surfaced from New rather than during request handling.
var (
	ErrNoLimiter = errors.New("middleware: Config.Limiter is required")
	ErrNoPolicy  = errors.New("middleware: Config.Policy is required")
)

// New returns net/http middleware enforcing the configured rate limits. On
// every request it sets X-RateLimit-Limit and X-RateLimit-Remaining; when a
// request is denied it responds with 429 and a Retry-After header.
func New(cfg Config) (func(http.Handler) http.Handler, error) {
	if cfg.Limiter == nil {
		return nil, ErrNoLimiter
	}
	if cfg.Policy == nil {
		return nil, ErrNoPolicy
	}
	if cfg.Key == nil {
		cfg.Key = ClientIP
	}
	if cfg.OnDenied == nil {
		cfg.OnDenied = http.HandlerFunc(writeDefaultDenied)
	}

	mw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rate := cfg.Policy(r)
			d := cfg.Limiter.Allow(cfg.Key(r), rate)

			h := w.Header()
			h.Set("X-RateLimit-Limit", strconv.Itoa(rate.Capacity))
			h.Set("X-RateLimit-Remaining", strconv.Itoa(d.Remaining))

			if !d.Allowed {
				h.Set("Retry-After", strconv.Itoa(retryAfterSeconds(d)))
				cfg.OnDenied.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	return mw, nil
}

// writeDefaultDenied is the fallback rejection response: a plain-text 429. The
// rate-limit and Retry-After headers have already been set by the middleware.
func writeDefaultDenied(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
}

// retryAfterSeconds converts the Decision's RetryAfter to whole seconds, rounded
// up, with a floor of 1 — the Retry-After header is expressed in seconds and a
// denied caller should always be told to wait at least one.
func retryAfterSeconds(d ratelimiter.Decision) int {
	secs := int(math.Ceil(d.RetryAfter.Seconds()))
	if secs < 1 {
		return 1
	}
	return secs
}

// ClientIP is the default KeyFunc: it keys on the client's IP address, taken
// from RemoteAddr with the port stripped.
//
// Note: behind a reverse proxy every request shares the proxy's IP, so a real
// deployment would parse a trusted X-Forwarded-For. That is intentionally left
// out here — blindly trusting that header lets clients spoof their key, so it
// needs deployment-specific trust configuration. See DESIGN.md.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// FixedRate returns a PolicyFunc that applies the same rate to every request —
// the common case of a single global limit.
func FixedRate(rate ratelimiter.Rate) PolicyFunc {
	return func(*http.Request) ratelimiter.Rate { return rate }
}
