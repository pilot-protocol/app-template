package broker

import "sync"

// Store records per-(app, caller) usage and enforces the call quota. It is the
// metering seam: the in-memory impl below is the default; a durable/shared impl
// (sqlite, redis) plugs in behind the same interface for prod (survives restart,
// scales across broker instances).
type Store interface {
	// Admit atomically checks the per-caller call quota and, if under, counts
	// the call. quota <= 0 means unlimited. Atomicity matters: concurrent calls
	// must not both slip under the limit.
	Admit(app, caller string, quota int) (admitted bool, calls int)
	// AddCost adds the partner-reported cost (in cents) for (app, caller).
	AddCost(app, caller string, cents float64)
	// Usage returns the running totals for (app, caller).
	Usage(app, caller string) (calls int, cents float64)
}

type cell struct {
	calls int
	cents float64
}

// MemStore is the default in-memory Store (single instance, non-durable).
type MemStore struct {
	mu sync.Mutex
	m  map[string]*cell
}

func NewMemStore() *MemStore { return &MemStore{m: map[string]*cell{}} }

func key(app, caller string) string { return app + "|" + caller }

func (s *MemStore) Admit(app, caller string, quota int) (bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.m[key(app, caller)]
	if c == nil {
		c = &cell{}
		s.m[key(app, caller)] = c
	}
	if quota > 0 && c.calls >= quota {
		return false, c.calls
	}
	c.calls++
	return true, c.calls
}

func (s *MemStore) AddCost(app, caller string, cents float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.m[key(app, caller)]; c != nil {
		c.cents += cents
	}
}

func (s *MemStore) Usage(app, caller string) (int, float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.m[key(app, caller)]; c != nil {
		return c.calls, c.cents
	}
	return 0, 0
}

// Snapshot returns a copy of all usage cells, keyed "app|caller" (for /gw/usage).
func (s *MemStore) Snapshot() map[string]struct {
	Calls int     `json:"calls"`
	Cents float64 `json:"cents"`
} {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]struct {
		Calls int     `json:"calls"`
		Cents float64 `json:"cents"`
	}, len(s.m))
	for k, c := range s.m {
		out[k] = struct {
			Calls int     `json:"calls"`
			Cents float64 `json:"cents"`
		}{c.calls, c.cents}
	}
	return out
}
