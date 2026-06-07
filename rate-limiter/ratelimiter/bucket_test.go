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
	// Given a fresh bucket (starts full at capacity 10)
	b := newBucket(rate10PerSec(), base)

	// When the first request arrives
	d := b.allow(base)

	// Then it is allowed and reports 9 tokens left
	if !d.Allowed {
		t.Fatal("a fresh bucket should allow the first request")
	}
	if d.Remaining != 9 {
		t.Fatalf("Remaining = %d, want 9 (10 capacity minus this request)", d.Remaining)
	}
}

func TestBucket_ExhaustsThenDenies(t *testing.T) {
	// Given a fresh bucket of capacity 10
	b := newBucket(rate10PerSec(), base)

	// When its 10 tokens are consumed at the same instant (no refill)
	for i := 0; i < 10; i++ {
		if d := b.allow(base); !d.Allowed {
			t.Fatalf("request %d should be allowed within capacity 10", i+1)
		}
	}

	// Then the 11th request is denied with 0 left and a 100ms Retry-After
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
	// Given a drained bucket (rate is 10/s = 1 token every 100ms)
	b := newBucket(rate10PerSec(), base)
	drain(b, base)

	// When 250ms elapse, exactly 2.5 tokens accrue
	at := base.Add(250 * time.Millisecond)

	// Then the first two requests are allowed and the third is denied
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
	// Given a drained bucket
	b := newBucket(rate10PerSec(), base)
	drain(b, base)

	// When it sits idle for an hour and is then refilled
	b.refill(base.Add(time.Hour))

	// Then tokens are capped at capacity (10), not accrued beyond it
	if b.tokens != 10 {
		t.Fatalf("tokens = %v, want capped at capacity 10", b.tokens)
	}
}

func TestBucket_RefillIsIdempotentAtSameInstant(t *testing.T) {
	// Given a drained bucket
	b := newBucket(rate10PerSec(), base)
	drain(b, base)

	// When refill is called again at the same instant (no time elapsed)
	b.refill(base)

	// Then no tokens are added (still 0)
	if b.tokens != 0 {
		t.Fatalf("tokens = %v, want 0 (no elapsed time means no refill)", b.tokens)
	}
}

func TestBucket_RetryAfterReflectsPartialToken(t *testing.T) {
	// Given a drained bucket
	b := newBucket(rate10PerSec(), base)
	drain(b, base)

	// When a request arrives 50ms later (only 0.5 tokens have accrued)
	d := b.allow(base.Add(50 * time.Millisecond))

	// Then it is denied, and Retry-After is the remaining 50ms to a full token
	if d.Allowed {
		t.Fatal("0.5 tokens must not satisfy a request")
	}
	if want := 50 * time.Millisecond; d.RetryAfter != want {
		t.Fatalf("RetryAfter = %v, want %v", d.RetryAfter, want)
	}
}
