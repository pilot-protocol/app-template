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
