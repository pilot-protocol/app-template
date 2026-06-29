package broker

import (
	"testing"
	"time"
)

func TestBreaker_OpensAfterThresholdAndRecovers(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	b := &Breaker{Threshold: 3, Cooldown: time.Minute, Now: func() time.Time { return clock }}

	if !b.Allow() {
		t.Fatal("fresh breaker should allow")
	}
	b.Record(false)
	b.Record(false)
	if !b.Allow() {
		t.Fatal("breaker should still be closed before threshold")
	}
	b.Record(false) // 3rd failure → opens
	if b.Allow() {
		t.Fatal("breaker should be open after threshold failures")
	}

	// Before cooldown elapses: still open.
	clock = clock.Add(30 * time.Second)
	if b.Allow() {
		t.Fatal("breaker should stay open during cooldown")
	}
	// After cooldown: half-open, allows a trial.
	clock = clock.Add(31 * time.Second)
	if !b.Allow() {
		t.Fatal("breaker should half-open after cooldown")
	}
	// A success closes it.
	b.Record(true)
	if !b.Allow() {
		t.Fatal("breaker should be closed after a successful trial")
	}
}

// TestBreaker_HalfOpenAllowsExactlyOneTrial guards the half-open state: after
// the cooldown only ONE probe may pass; concurrent callers stay denied until the
// probe's outcome is recorded, so a flapping upstream isn't hit by a herd.
func TestBreaker_HalfOpenAllowsExactlyOneTrial(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	b := &Breaker{Threshold: 1, Cooldown: time.Minute, Now: func() time.Time { return clock }}

	b.Record(false) // opens immediately (threshold 1)
	if b.Allow() {
		t.Fatal("breaker should be open")
	}

	clock = clock.Add(2 * time.Minute) // cooldown elapsed
	if !b.Allow() {
		t.Fatal("first post-cooldown call should be the half-open probe")
	}
	if b.Allow() {
		t.Fatal("second concurrent call must be denied while the probe is in flight")
	}

	// The probe fails → re-open for another full cooldown (no immediate retry).
	b.Record(false)
	if b.Allow() {
		t.Fatal("a failed probe must re-open the breaker")
	}
	clock = clock.Add(2 * time.Minute)
	if !b.Allow() {
		t.Fatal("a fresh probe is allowed after the next cooldown")
	}
	// This probe succeeds → fully closed: every subsequent call is allowed.
	b.Record(true)
	first, second := b.Allow(), b.Allow()
	if !first || !second {
		t.Fatal("breaker should be fully closed after a successful probe")
	}
}

func TestBreaker_DisabledWhenThresholdZero(t *testing.T) {
	var b *Breaker // nil breaker must be safe
	if !b.Allow() {
		t.Fatal("nil breaker should allow")
	}
	b.Record(false) // must not panic

	b2 := &Breaker{Threshold: 0}
	for i := 0; i < 100; i++ {
		b2.Record(false)
	}
	if !b2.Allow() {
		t.Fatal("threshold 0 disables the breaker")
	}
}
