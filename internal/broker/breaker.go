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
	if b.now().Sub(b.openedAt) >= b.Cooldown {
		return true // half-open: allow one trial
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
		return
	}
	b.failures++
	if b.failures >= b.Threshold {
		b.open = true
		b.openedAt = b.now()
	}
}
