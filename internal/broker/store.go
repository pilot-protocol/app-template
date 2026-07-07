package broker

import (
	"errors"
	"sync"
	"time"
)

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

// ProvisionStore records per-user provisioning (identity + source IP + credit
// ledger) for provisioned apps (e.g. io.pilot.smol). It is separate from Store so
// existing managed apps are unaffected; the SQLite and in-memory stores satisfy
// both. All methods are atomic — concurrent first-use must seed exactly once and
// concurrent debits must never over-spend.
type ProvisionStore interface {
	// Provision is idempotent by (app, caller): on FIRST sight it records
	// (ip, first_seen, last_mint=now) and seeds `seed` credits; on repeat it
	// refreshes last_mint and returns the existing record WITHOUT re-seeding.
	//
	// Before a first-time insert it enforces spam controls:
	//   - per-IP identity cap: if this app+ip already has >= maxPerIP DISTINCT
	//     callers, reject with ErrIPCap (Sybil / key-farming guard);
	//   - per-identity mint cooldown: if the same caller re-mints within
	//     `cooldown`, reject with ErrCooldown.
	// maxPerIP <= 0 disables the cap; cooldown <= 0 disables the cooldown.
	Provision(app, caller, ip string, seed, maxPerIP int, cooldown time.Duration, now time.Time) (ProvisionRecord, error)

	// Credit returns the caller's current credit balance (0 if unprovisioned).
	Credit(app, caller string) (int, error)

	// Debit atomically reserves n credits: if the balance is >= n it subtracts
	// them and returns ok=true; otherwise it changes nothing and returns
	// ok=false (the caller responds 402). n <= 0 is a no-op returning ok=true.
	Debit(app, caller string, n int) (ok bool, remaining int, err error)

	// Refund returns n credits reserved by a Debit whose downstream op ultimately
	// failed, so a failed cloud push never burns credit. It never overflows past
	// what was debited in practice (callers only refund what they reserved).
	Refund(app, caller string, n int)

	// Settle debits n credits AFTER the fact, clamped to the available balance so
	// it never rejects and never drives the balance below zero. Unlike Debit (a
	// pre-flight reservation that fails when funds are short), Settle is used by the
	// response-cost credit path where the true cost is only known from the partner
	// response: the call already ran, so the debit must always apply. It returns the
	// remaining balance. n <= 0 and an unprovisioned caller are no-ops.
	Settle(app, caller string, n int) (remaining int, err error)

	// Get returns the caller's provision record (for reading the current rotation
	// counter + credit). exists is false for an unprovisioned caller.
	Get(app, caller string) (rec ProvisionRecord, exists bool, err error)

	// Rotate increments the caller's per-user rotation counter and returns the
	// updated record. It does NOT touch credit — a rotated key keeps the same
	// balance. Rotating an unprovisioned caller is a no-op (exists=false).
	Rotate(app, caller string) (rec ProvisionRecord, exists bool, err error)
}

// ProvisionRecord is a per-user provisioning row.
type ProvisionRecord struct {
	App       string    `json:"app"`
	Caller    string    `json:"caller"`
	IP        string    `json:"ip"`
	FirstSeen time.Time `json:"first_seen"`
	LastMint  time.Time `json:"last_mint"`
	Credits   int       `json:"credits"`
	Rot       int       `json:"rot"` // per-user rotation counter (bumped by Rotate)
	New       bool      `json:"new"` // true when this call created (seeded) the row
}

var (
	// ErrIPCap is returned by Provision when the per-IP identity cap is hit.
	ErrIPCap = errors.New("broker: per-IP identity cap exceeded")
	// ErrCooldown is returned by Provision when a caller re-mints too soon.
	ErrCooldown = errors.New("broker: provision cooldown not elapsed")
)

type cell struct {
	calls int
	cents float64
}

// MemStore is the default in-memory Store + ProvisionStore (single instance,
// non-durable). Used in dev and tests; prod uses SQLiteStore for durability.
type MemStore struct {
	mu   sync.Mutex
	m    map[string]*cell
	prov map[string]*ProvisionRecord // keyed "app|caller"
}

func NewMemStore() *MemStore {
	return &MemStore{m: map[string]*cell{}, prov: map[string]*ProvisionRecord{}}
}

func (s *MemStore) Provision(app, caller, ip string, seed, maxPerIP int, cooldown time.Duration, now time.Time) (ProvisionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(app, caller)
	if rec := s.prov[k]; rec != nil { // repeat: cooldown, refresh, no re-seed
		if cooldown > 0 && now.Sub(rec.LastMint) < cooldown {
			return ProvisionRecord{}, ErrCooldown
		}
		rec.LastMint = now
		out := *rec
		out.New = false
		return out, nil
	}
	// first-time insert: enforce per-IP identity cap over DISTINCT callers.
	if maxPerIP > 0 {
		distinct := 0
		for _, rec := range s.prov {
			if rec.App == app && rec.IP == ip {
				distinct++
			}
		}
		if distinct >= maxPerIP {
			return ProvisionRecord{}, ErrIPCap
		}
	}
	rec := &ProvisionRecord{App: app, Caller: caller, IP: ip, FirstSeen: now, LastMint: now, Credits: seed}
	s.prov[k] = rec
	out := *rec
	out.New = true
	return out, nil
}

func (s *MemStore) Credit(app, caller string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec := s.prov[key(app, caller)]; rec != nil {
		return rec.Credits, nil
	}
	return 0, nil
}

func (s *MemStore) Debit(app, caller string, n int) (bool, int, error) {
	if n <= 0 {
		c, _ := s.Credit(app, caller)
		return true, c, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.prov[key(app, caller)]
	if rec == nil || rec.Credits < n {
		rem := 0
		if rec != nil {
			rem = rec.Credits
		}
		return false, rem, nil
	}
	rec.Credits -= n
	return true, rec.Credits, nil
}

func (s *MemStore) Refund(app, caller string, n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec := s.prov[key(app, caller)]; rec != nil {
		rec.Credits += n
	}
}

func (s *MemStore) Settle(app, caller string, n int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.prov[key(app, caller)]
	if rec == nil {
		return 0, nil
	}
	if n > 0 {
		if n > rec.Credits {
			n = rec.Credits // clamp: floor the balance at zero
		}
		rec.Credits -= n
	}
	return rec.Credits, nil
}

func (s *MemStore) Get(app, caller string) (ProvisionRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec := s.prov[key(app, caller)]; rec != nil {
		return *rec, true, nil
	}
	return ProvisionRecord{}, false, nil
}

func (s *MemStore) Rotate(app, caller string) (ProvisionRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.prov[key(app, caller)]
	if rec == nil {
		return ProvisionRecord{}, false, nil
	}
	rec.Rot++ // credit untouched
	return *rec, true, nil
}

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
