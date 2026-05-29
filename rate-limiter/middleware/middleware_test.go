package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
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
	h := newStack(t, 2)

	for i := 0; i < 2; i++ {
		rec := call(h, "1.2.3.4")
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: code %d, want 200", i+1, rec.Code)
		}
		if rec.Body.String() != "ok" {
			t.Fatalf("request %d: body %q, want passthrough %q", i+1, rec.Body.String(), "ok")
		}
	}

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
	h := newStack(t, 2)

	rec := call(h, "9.9.9.9")

	if got := rec.Header().Get("X-RateLimit-Limit"); got != "2" {
		t.Fatalf("X-RateLimit-Limit = %q, want 2", got)
	}
	if got := rec.Header().Get("X-RateLimit-Remaining"); got != "1" {
		t.Fatalf("X-RateLimit-Remaining = %q, want 1 (one of two consumed)", got)
	}
}

func TestMiddleware_LimitsPerClientIP(t *testing.T) {
	h := newStack(t, 1)

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

func TestNew_RequiresLimiterAndPolicy(t *testing.T) {
	rate := ratelimiter.Rate{Capacity: 1, Interval: time.Second}

	if _, err := New(Config{Policy: FixedRate(rate)}); err != ErrNoLimiter {
		t.Fatalf("nil Limiter: err = %v, want ErrNoLimiter", err)
	}

	lim := ratelimiter.New(ratelimiter.Options{SweepInterval: -1})
	t.Cleanup(func() { lim.Close() })
	if _, err := New(Config{Limiter: lim}); err != ErrNoPolicy {
		t.Fatalf("nil Policy: err = %v, want ErrNoPolicy", err)
	}
}
