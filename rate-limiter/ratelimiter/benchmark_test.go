package ratelimiter

import (
	"testing"
	"time"
)

// These benchmarks back the concurrency decision in DESIGN.md: they measure the
// single-mutex hot path so the "shard only if measured" claim rests on numbers,
// not assumption. A huge capacity keeps every call on the allow path so we time
// the lock + refill arithmetic rather than the denial branch.

func benchRate() Rate { return Rate{Capacity: 1_000_000_000, Interval: time.Second} }

// BenchmarkAllow_Serial measures the uncontended cost of a single Allow.
func BenchmarkAllow_Serial(b *testing.B) {
	lim := New(Options{SweepInterval: -1})
	defer lim.Close()
	rate := benchRate()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lim.Allow("k", rate)
	}
}

// BenchmarkAllow_Parallel measures Allow under contention on a single hot key —
// the worst case for one global mutex. Compare against -cpu values to see how
// throughput holds up as cores are added.
func BenchmarkAllow_Parallel(b *testing.B) {
	lim := New(Options{SweepInterval: -1})
	defer lim.Close()
	rate := benchRate()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			lim.Allow("k", rate)
		}
	})
}
