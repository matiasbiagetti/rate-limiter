package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/biagettimati/rate-limiter/ratelimiter"
)

// newStack builds a middleware-wrapped handler over a limiter with a fixed
// clock (no refill during a test) and a single global rate of the given
// capacity. The wrapped handler writes "ok" on 200.
func newStack(t *testing.T, capacity int) http.Handler {
	t.Helper()
	lim := ratelimiter.New(ratelimiter.Options{
		Clock:         ratelimiter.NewManualClock(time.Unix(0, 0)),
		SweepInterval: -1, // janitor off
	})
	t.Cleanup(func() { lim.Close() })

	rate := ratelimiter.Rate{Capacity: capacity, Interval: time.Second}
	mw, err := New(Config{Limiter: lim, Policy: FixedRate(rate)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok")
	})
	return mw(next)
}

// call issues a GET from the given client IP and returns the recorded response.
func call(h http.Handler, ip string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = ip + ":12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMiddleware_AllowsUpToLimitThenBlocks(t *testing.T) {
	// Given a middleware stack with a 2-per-second limit
	h := newStack(t, 2)

	// When the same IP makes its first two requests
	// Then both pass through to the handler (200, body "ok")
	for i := 0; i < 2; i++ {
		rec := call(h, "1.2.3.4")
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: code %d, want 200", i+1, rec.Code)
		}
		if rec.Body.String() != "ok" {
			t.Fatalf("request %d: body %q, want passthrough %q", i+1, rec.Body.String(), "ok")
		}
	}

	// When the same IP makes a third request
	// Then it is rejected with 429, a Retry-After header, and 0 remaining
	rec := call(h, "1.2.3.4")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd request: code %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 response should carry a Retry-After header")
	}
	if got := rec.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Fatalf("X-RateLimit-Remaining = %q, want 0", got)
	}
}

func TestMiddleware_SetsRateLimitHeadersOnAllow(t *testing.T) {
	// Given a stack with capacity 2
	h := newStack(t, 2)

	// When one request is made
	rec := call(h, "9.9.9.9")

	// Then the rate-limit headers report the limit and the remaining tokens
	if got := rec.Header().Get("X-RateLimit-Limit"); got != "2" {
		t.Fatalf("X-RateLimit-Limit = %q, want 2", got)
	}
	if got := rec.Header().Get("X-RateLimit-Remaining"); got != "1" {
		t.Fatalf("X-RateLimit-Remaining = %q, want 1 (one of two consumed)", got)
	}
}

func TestMiddleware_LimitsPerClientIP(t *testing.T) {
	// Given a stack with capacity 1
	h := newStack(t, 1)

	// When two different IPs each make one request, then the first IP repeats
	// Then both first requests pass and the repeat is denied (per-IP buckets)
	if rec := call(h, "10.0.0.1"); rec.Code != http.StatusOK {
		t.Fatalf("client A first request: code %d, want 200", rec.Code)
	}
	if rec := call(h, "10.0.0.2"); rec.Code != http.StatusOK {
		t.Fatalf("client B first request: code %d, want 200 (independent IP)", rec.Code)
	}
	if rec := call(h, "10.0.0.1"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("client A second request: code %d, want 429", rec.Code)
	}
}

func TestMiddleware_OnDeniedCustomisesRejection(t *testing.T) {
	// Given a stack with capacity 1 and a custom OnDenied that writes JSON
	lim := ratelimiter.New(ratelimiter.Options{
		Clock:         ratelimiter.NewManualClock(time.Unix(0, 0)),
		SweepInterval: -1,
	})
	t.Cleanup(func() { lim.Close() })

	rate := ratelimiter.Rate{Capacity: 1, Interval: time.Second}
	mw, err := New(Config{
		Limiter: lim,
		Policy:  FixedRate(rate),
		OnDenied: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			// The middleware has already set Retry-After; a custom handler can
			// read it and shape the body however it likes.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":"rate_limited","retryAfter":"`+w.Header().Get("Retry-After")+`"}`)
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") }))

	// When the only token is consumed and a second request is denied
	call(h, "1.1.1.1")        // consume the only token
	rec := call(h, "1.1.1.1") // this one is denied

	// Then the 429 carries the custom JSON body plus the Retry-After the middleware set
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code %d, want 429", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type %q, want application/json from the custom handler", ct)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("middleware should still set Retry-After before delegating to OnDenied")
	}
	if !strings.Contains(rec.Body.String(), "rate_limited") {
		t.Fatalf("body %q should be the custom JSON payload", rec.Body.String())
	}
}

func TestNew_RequiresLimiterAndPolicy(t *testing.T) {
	// Given configs that are missing a required field
	rate := ratelimiter.Rate{Capacity: 1, Interval: time.Second}

	// When New is called without a Limiter
	// Then it returns ErrNoLimiter
	if _, err := New(Config{Policy: FixedRate(rate)}); err != ErrNoLimiter {
		t.Fatalf("nil Limiter: err = %v, want ErrNoLimiter", err)
	}

	// When New is called without a Policy
	// Then it returns ErrNoPolicy
	lim := ratelimiter.New(ratelimiter.Options{SweepInterval: -1})
	t.Cleanup(func() { lim.Close() })
	if _, err := New(Config{Limiter: lim}); err != ErrNoPolicy {
		t.Fatalf("nil Policy: err = %v, want ErrNoPolicy", err)
	}
}

func TestClientIP_StripsPortAndHandlesAddressForms(t *testing.T) {
	// Given RemoteAddr values in the different shapes net/http can produce
	cases := []struct {
		name       string
		remoteAddr string
		want       string
	}{
		{"ipv4 with port", "1.2.3.4:5678", "1.2.3.4"},
		{"ipv6 with port", "[::1]:1234", "::1"},
		{"no port falls back to the raw value", "1.2.3.4", "1.2.3.4"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// When ClientIP reads the address
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remoteAddr

			// Then the port is stripped, or the raw value is returned when there is none
			if got := ClientIP(req); got != tc.want {
				t.Fatalf("ClientIP(%q) = %q, want %q", tc.remoteAddr, got, tc.want)
			}
		})
	}
}

func TestClientIP_IgnoresXForwardedFor(t *testing.T) {
	// Given a request carrying a spoofable X-Forwarded-For header
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "1.2.3.4:5678"
	req.Header.Set("X-Forwarded-For", "9.9.9.9")

	// When ClientIP extracts the limiting key
	// Then it uses RemoteAddr and ignores the header, so a client cannot spoof its key
	// by setting X-Forwarded-For (trusting it needs deployment-specific config; see DESIGN.md).
	if got := ClientIP(req); got != "1.2.3.4" {
		t.Fatalf("ClientIP = %q, want 1.2.3.4 (X-Forwarded-For must be ignored to prevent key spoofing)", got)
	}
}
