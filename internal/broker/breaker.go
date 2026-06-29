package broker

import (
	"sync"
	"time"
)

// Breaker is a per-app circuit breaker that protects the master key (and the
// partner) from a flapping upstream. After Threshold consecutive failures it
// opens for Cooldown; while open, calls fail fast without hitting the partner.
// A single trial call after the cooldown closes it again on success.
//
// Threshold <= 0 disables the breaker (Allow always true, Record a no-op).
type Breaker struct {
	Threshold int
	Cooldown  time.Duration
	Now       func() time.Time

	mu       sync.Mutex
	failures int
	openedAt time.Time
	open     bool
	probing  bool // a single half-open trial call is in flight
}

func (b *Breaker) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// Allow reports whether a call may proceed. When the breaker is open it stays
// closed-to-traffic until the cooldown elapses, then permits one trial call.
func (b *Breaker) Allow() bool {
	if b == nil || b.Threshold <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.open {
		return true
	}
	// Half-open: once the cooldown elapses permit exactly ONE trial call (the
	// probe). Concurrent callers are still denied until Record reports the
	// probe's outcome, so a flapping upstream isn't hit by a thundering herd.
	if b.now().Sub(b.openedAt) >= b.Cooldown && !b.probing {
		b.probing = true
		return true
	}
	return false
}

// Record feeds the outcome of a call back to the breaker.
func (b *Breaker) Record(success bool) {
	if b == nil || b.Threshold <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if success {
		b.failures = 0
		b.open = false
		b.probing = false
		return
	}
	b.failures++
	if b.open {
		// A half-open probe failed — stay open and restart the cooldown so the
		// next probe waits a full Cooldown again.
		b.probing = false
		b.openedAt = b.now()
		return
	}
	if b.failures >= b.Threshold {
		b.open = true
		b.openedAt = b.now()
	}
}
