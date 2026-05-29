package ratelimiter

import (
	"testing"
	"time"
)

// rate10PerSec is the fixture limit used across bucket tests: 10 tokens
// refilled over one second, i.e. one token every 100ms.
func rate10PerSec() Rate { return Rate{Capacity: 10, Interval: time.Second} }

// A fixed, readable base instant. Tests advance time by passing explicit
// timestamps to the bucket, so no real clock or sleeps are involved.
var base = time.Unix(0, 0)

// drain consumes the whole bucket at instant t, leaving exactly zero tokens.
func drain(b *tokenBucket, t time.Time) {
	for i := 0; i < b.rate.Capacity; i++ {
		b.allow(t)
	}
}

func TestBucket_StartsFull(t *testing.T) {
	b := newBucket(rate10PerSec(), base)

	d := b.allow(base)

	if !d.Allowed {
		t.Fatal("a fresh bucket should allow the first request")
	}
	if d.Remaining != 9 {
		t.Fatalf("Remaining = %d, want 9 (10 capacity minus this request)", d.Remaining)
	}
}

func TestBucket_ExhaustsThenDenies(t *testing.T) {
	b := newBucket(rate10PerSec(), base)

	for i := 0; i < 10; i++ {
		if d := b.allow(base); !d.Allowed {
			t.Fatalf("request %d should be allowed within capacity 10", i+1)
		}
	}

	d := b.allow(base)
	if d.Allowed {
		t.Fatal("the 11th request with no refill should be denied")
	}
	if d.Remaining != 0 {
		t.Fatalf("Remaining = %d, want 0", d.Remaining)
	}
	if want := 100 * time.Millisecond; d.RetryAfter != want {
		t.Fatalf("RetryAfter = %v, want %v (time to accrue one token)", d.RetryAfter, want)
	}
}

func TestBucket_RefillsProportionalToElapsedTime(t *testing.T) {
	b := newBucket(rate10PerSec(), base)
	drain(b, base)

	// 250ms later, exactly 2.5 tokens have accrued: two requests succeed, the
	// third fails.
	at := base.Add(250 * time.Millisecond)
	if !b.allow(at).Allowed {
		t.Fatal("1st request after 250ms should be allowed (2.5 tokens accrued)")
	}
	if !b.allow(at).Allowed {
		t.Fatal("2nd request after 250ms should be allowed")
	}
	if b.allow(at).Allowed {
		t.Fatal("3rd request after 250ms should be denied (only 2.5 tokens)")
	}
}

func TestBucket_NeverExceedsCapacity(t *testing.T) {
	b := newBucket(rate10PerSec(), base)
	drain(b, base)

	// An arbitrarily long idle period must not accrue beyond capacity.
	b.refill(base.Add(time.Hour))

	if b.tokens != 10 {
		t.Fatalf("tokens = %v, want capped at capacity 10", b.tokens)
	}
}

func TestBucket_RefillIsIdempotentAtSameInstant(t *testing.T) {
	b := newBucket(rate10PerSec(), base)
	drain(b, base)

	b.refill(base) // no time has elapsed since the drain

	if b.tokens != 0 {
		t.Fatalf("tokens = %v, want 0 (no elapsed time means no refill)", b.tokens)
	}
}

func TestBucket_RetryAfterReflectsPartialToken(t *testing.T) {
	b := newBucket(rate10PerSec(), base)
	drain(b, base)

	// 50ms later only 0.5 tokens exist: still denied, and the caller should be
	// told to retry after the remaining 50ms needed to reach a full token.
	d := b.allow(base.Add(50 * time.Millisecond))

	if d.Allowed {
		t.Fatal("0.5 tokens must not satisfy a request")
	}
	if want := 50 * time.Millisecond; d.RetryAfter != want {
		t.Fatalf("RetryAfter = %v, want %v", d.RetryAfter, want)
	}
}
