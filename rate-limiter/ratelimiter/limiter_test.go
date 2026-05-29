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
	lim := newTestLimiter(NewManualClock(base))
	defer lim.Close()
	rate := Rate{Capacity: 3, Interval: time.Second}

	for i := 0; i < 3; i++ {
		if !lim.Allow("a", rate).Allowed {
			t.Fatalf("request %d for key a should be allowed within capacity 3", i+1)
		}
	}
	if lim.Allow("a", rate).Allowed {
		t.Fatal("4th request for key a should be denied")
	}
}

func TestLimiter_KeysAreIndependent(t *testing.T) {
	lim := newTestLimiter(NewManualClock(base))
	defer lim.Close()
	rate := Rate{Capacity: 1, Interval: time.Second}

	if !lim.Allow("a", rate).Allowed {
		t.Fatal("first request for a should pass")
	}
	if !lim.Allow("b", rate).Allowed {
		t.Fatal("first request for b should pass — keys are independent")
	}
	if lim.Allow("a", rate).Allowed {
		t.Fatal("second request for a should be denied")
	}
}

func TestLimiter_AdoptsChangedRate(t *testing.T) {
	clk := NewManualClock(base)
	lim := newTestLimiter(clk)
	defer lim.Close()

	small := Rate{Capacity: 2, Interval: time.Second}
	lim.Allow("a", small)
	lim.Allow("a", small)
	if lim.Allow("a", small).Allowed {
		t.Fatal("key a should be drained under the small policy")
	}

	// Upgrade to a larger policy. After 100ms the bucket should have accrued a
	// token at the new (faster) rate and admit a request.
	big := Rate{Capacity: 10, Interval: time.Second}
	clk.Advance(100 * time.Millisecond)
	if !lim.Allow("a", big).Allowed {
		t.Fatal("after upgrading the rate and 100ms, a request should be allowed")
	}
}

// TestLimiter_ConcurrentAllow_NoOverAdmission is the concurrency-correctness
// proof. Run with `go test -race`: the detector must report no data race, and
// because the clock is fixed (no refill), the total admitted must equal the
// capacity exactly — any over-admission would indicate a lost update in the
// refill/check/consume critical section.
func TestLimiter_ConcurrentAllow_NoOverAdmission(t *testing.T) {
	lim := newTestLimiter(NewManualClock(base))
	defer lim.Close()
	rate := Rate{Capacity: 100, Interval: time.Hour}

	const goroutines, perGoroutine = 64, 50 // 3200 attempts against one key
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

	if admitted != 100 {
		t.Fatalf("admitted %d, want exactly 100 (capacity); over-admission means a race", admitted)
	}
}

func TestLimiter_EvictsOnlyFullyRefilledBuckets(t *testing.T) {
	clk := NewManualClock(base)
	lim := newTestLimiter(clk)
	defer lim.Close()
	rate := Rate{Capacity: 5, Interval: time.Second}

	lim.Allow("a", rate)

	// Idle for less than one Interval: not yet refilled, must not be evicted.
	clk.Advance(500 * time.Millisecond)
	lim.sweep()
	if got := lim.Len(); got != 1 {
		t.Fatalf("bucket evicted too early: Len = %d, want 1", got)
	}

	// Idle beyond one Interval: fully refilled, safe to evict losslessly.
	clk.Advance(time.Second)
	lim.sweep()
	if got := lim.Len(); got != 0 {
		t.Fatalf("fully refilled bucket should be evicted: Len = %d, want 0", got)
	}
}
