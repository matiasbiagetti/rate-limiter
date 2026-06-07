package ratelimiter

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestLimiter builds a limiter with a controllable clock and the janitor
// disabled, so eviction is driven explicitly via sweep() in tests.
func newTestLimiter(clk Clock) *Limiter {
	return New(Options{Clock: clk, SweepInterval: -1})
}

func TestLimiter_AllowsUpToCapacityPerKey(t *testing.T) {
	// Given a limiter with a 3-per-second rate
	lim := newTestLimiter(NewManualClock(base))
	defer lim.Close()
	rate := Rate{Capacity: 3, Interval: time.Second}

	// When key "a" makes 3 requests at the same instant (no refill)
	for i := 0; i < 3; i++ {
		if !lim.Allow("a", rate).Allowed {
			t.Fatalf("request %d for key a should be allowed within capacity 3", i+1)
		}
	}

	// Then the 4th is denied
	if lim.Allow("a", rate).Allowed {
		t.Fatal("4th request for key a should be denied")
	}
}

func TestLimiter_KeysAreIndependent(t *testing.T) {
	// Given a limiter with a capacity of 1 per key
	lim := newTestLimiter(NewManualClock(base))
	defer lim.Close()
	rate := Rate{Capacity: 1, Interval: time.Second}

	// When "a" and then "b" each make one request, then "a" makes a second
	// Then a passes, b passes (independent bucket), and a's second is denied
	if !lim.Allow("a", rate).Allowed {
		t.Fatal("first request for a should pass")
	}
	if !lim.Allow("b", rate).Allowed {
		t.Fatal("first request for b should pass; keys are independent")
	}
	if lim.Allow("a", rate).Allowed {
		t.Fatal("second request for a should be denied")
	}
}

func TestLimiter_AdoptsChangedRate(t *testing.T) {
	// Given key "a" drained under a small rate (capacity 2)
	clk := NewManualClock(base)
	lim := newTestLimiter(clk)
	defer lim.Close()
	small := Rate{Capacity: 2, Interval: time.Second}
	lim.Allow("a", small)
	lim.Allow("a", small)
	if lim.Allow("a", small).Allowed {
		t.Fatal("key a should be drained under the small policy")
	}

	// When the rate is upgraded to a larger one and 100ms pass
	big := Rate{Capacity: 10, Interval: time.Second}
	clk.Advance(100 * time.Millisecond)

	// Then a request is allowed (a token accrued at the new, faster rate)
	if !lim.Allow("a", big).Allowed {
		t.Fatal("after upgrading the rate and 100ms, a request should be allowed")
	}
}

// TestLimiter_ConcurrentAllow_NoOverAdmission is the concurrency-correctness
// proof. Run with `go test -race`: the detector must report no data race, and
// because the clock is fixed (no refill), the total admitted must equal the
// capacity exactly; any over-admission would indicate a lost update in the
// refill/check/consume critical section.
func TestLimiter_ConcurrentAllow_NoOverAdmission(t *testing.T) {
	// Given a fixed clock (no refill) and a capacity of 100 on one hot key
	lim := newTestLimiter(NewManualClock(base))
	defer lim.Close()
	rate := Rate{Capacity: 100, Interval: time.Hour}

	// When 64 goroutines hammer that key concurrently (3,200 attempts total)
	const goroutines, perGoroutine = 64, 50
	var admitted int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				if lim.Allow("hot", rate).Allowed {
					atomic.AddInt64(&admitted, 1)
				}
			}
		}()
	}
	wg.Wait()

	// Then exactly 100 are admitted (over-admission would mean a race)
	if admitted != 100 {
		t.Fatalf("admitted %d, want exactly 100 (capacity); over-admission means a race", admitted)
	}
}

func TestLimiter_InvalidRateFailsClosed(t *testing.T) {
	// Given a limiter
	lim := newTestLimiter(NewManualClock(base))
	defer lim.Close()

	// When requests arrive with invalid rates (zero interval, then zero capacity)
	// Then both are denied (fail closed) and no bucket is created
	if lim.Allow("a", Rate{Capacity: 5, Interval: 0}).Allowed {
		t.Fatal("rate with zero interval must fail closed (deny), not bypass the limiter")
	}
	if lim.Allow("b", Rate{Capacity: 0, Interval: time.Second}).Allowed {
		t.Fatal("rate with zero capacity must be denied")
	}
	if got := lim.Len(); got != 0 {
		t.Fatalf("invalid rates should not create buckets: Len = %d, want 0", got)
	}
}

func TestLimiter_AdoptsSmallerRateClampsTokens(t *testing.T) {
	// Given key "a" with one request under a large rate (capacity 10, 9 left)
	lim := newTestLimiter(NewManualClock(base))
	defer lim.Close()
	big := Rate{Capacity: 10, Interval: time.Second}
	if d := lim.Allow("a", big); d.Remaining != 9 {
		t.Fatalf("Remaining = %d, want 9 under the large rate", d.Remaining)
	}

	// When the rate switches to a smaller capacity (2)
	small := Rate{Capacity: 2, Interval: time.Second}

	// Then the surplus tokens are clamped to the new cap before consuming (1 left)
	if d := lim.Allow("a", small); d.Remaining != 1 {
		t.Fatalf("Remaining = %d, want 1 after clamping tokens to the smaller capacity", d.Remaining)
	}
}

func TestLimiter_CloseIsIdempotent(t *testing.T) {
	// Given a limiter with the background janitor running
	lim := New(Options{SweepInterval: 5 * time.Millisecond})

	// When Close is called twice
	// Then both calls return nil (closing the stop channel twice would panic)
	if err := lim.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := lim.Close(); err != nil {
		t.Fatalf("second Close should be a no-op: %v", err)
	}
}

// TestLimiter_JanitorEvictsInBackground exercises the real janitor goroutine
// (not sweep() directly): with a live clock and a fast sweep, an idle bucket is
// removed on its own. Uses a generous deadline to stay non-flaky.
func TestLimiter_JanitorEvictsInBackground(t *testing.T) {
	// Given a limiter with a fast background janitor and one fresh bucket
	lim := New(Options{SweepInterval: 5 * time.Millisecond})
	defer lim.Close()
	lim.Allow("a", Rate{Capacity: 1, Interval: 10 * time.Millisecond})

	// When enough time passes for the bucket to become idle (and refilled)
	// Then the janitor removes it on its own, so Len drops to 0
	deadline := time.After(2 * time.Second)
	for lim.Len() != 0 {
		select {
		case <-deadline:
			t.Fatal("janitor did not evict the idle bucket within 2s")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestLimiter_EvictsOnlyFullyRefilledBuckets(t *testing.T) {
	// Given key "a" with one request (capacity 5, 1s interval)
	clk := NewManualClock(base)
	lim := newTestLimiter(clk)
	defer lim.Close()
	rate := Rate{Capacity: 5, Interval: time.Second}
	lim.Allow("a", rate)

	// When idle for less than one interval and then swept
	// Then it is not evicted (not yet refilled to full)
	clk.Advance(500 * time.Millisecond)
	lim.sweep()
	if got := lim.Len(); got != 1 {
		t.Fatalf("bucket evicted too early: Len = %d, want 1", got)
	}

	// When idle beyond one interval and then swept
	// Then it is evicted (fully refilled, so dropping it is lossless)
	clk.Advance(time.Second)
	lim.sweep()
	if got := lim.Len(); got != 0 {
		t.Fatalf("fully refilled bucket should be evicted: Len = %d, want 0", got)
	}
}
