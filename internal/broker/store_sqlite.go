package broker

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo)
)

// SQLiteStore is a durable Store backed by SQLite. Usage survives broker
// restarts, and (on a shared volume / single writer) is consistent across
// instances. It satisfies the same Store contract as MemStore, so the broker
// is unchanged — only the constructor differs.
//
// Concurrency: we cap the pool at one connection so Admit's read-modify-write
// is serialized without explicit locking. A metering store is not the hot path,
// so this is the simplest correct choice.
type SQLiteStore struct{ db *sql.DB }

// OpenSQLiteStore opens (and migrates) a SQLite-backed store at path. Use
// ":memory:" for tests.
func OpenSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	db.SetMaxOpenConns(1)
	// WAL + synchronous=NORMAL: commits don't fsync on every write (only at
	// checkpoint), turning a fsync-bound few-hundred/sec into thousands/sec on
	// disk while staying crash-safe. busy_timeout avoids spurious "locked" errors.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite pragma %q: %w", pragma, err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS usage (
		app    TEXT NOT NULL,
		caller TEXT NOT NULL,
		calls  INTEGER NOT NULL DEFAULT 0,
		cents  REAL    NOT NULL DEFAULT 0,
		PRIMARY KEY (app, caller)
	)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite migrate: %w", err)
	}
	// provision: one row per (app, caller). ip + timestamps drive spam controls;
	// credits is the broker's own ledger (the master key can't read cloud usage).
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS provision (
		app        TEXT NOT NULL,
		caller     TEXT NOT NULL,
		ip         TEXT NOT NULL,
		first_seen INTEGER NOT NULL,
		last_mint  INTEGER NOT NULL,
		credits    INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (app, caller)
	)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite migrate provision: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS provision_app_ip ON provision(app, ip)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite migrate provision index: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Close() error { return s.db.Close() }

func (s *SQLiteStore) Admit(app, caller string, quota int) (bool, int) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, 0
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`INSERT OR IGNORE INTO usage (app, caller) VALUES (?, ?)`, app, caller); err != nil {
		return false, 0
	}
	var calls int
	if err := tx.QueryRow(`SELECT calls FROM usage WHERE app=? AND caller=?`, app, caller).Scan(&calls); err != nil {
		return false, 0
	}
	if quota > 0 && calls >= quota {
		return false, calls
	}
	if _, err := tx.Exec(`UPDATE usage SET calls=calls+1 WHERE app=? AND caller=?`, app, caller); err != nil {
		return false, calls
	}
	if err := tx.Commit(); err != nil {
		return false, calls
	}
	return true, calls + 1
}

func (s *SQLiteStore) AddCost(app, caller string, cents float64) {
	_, _ = s.db.Exec(`UPDATE usage SET cents=cents+? WHERE app=? AND caller=?`, cents, app, caller)
}

func (s *SQLiteStore) Usage(app, caller string) (int, float64) {
	var calls int
	var cents float64
	err := s.db.QueryRow(`SELECT calls, cents FROM usage WHERE app=? AND caller=?`, app, caller).Scan(&calls, &cents)
	if err != nil {
		return 0, 0
	}
	return calls, cents
}

// Provision is idempotent by (app, caller) and enforces the per-IP identity cap
// + per-identity cooldown. The single-conn pool serializes the whole tx, so the
// concurrent-first-use race resolves to exactly one seed.
func (s *SQLiteStore) Provision(app, caller, ip string, seed, maxPerIP int, cooldown time.Duration, now time.Time) (ProvisionRecord, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return ProvisionRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var firstSeen, lastMint int64
	var credits int
	err = tx.QueryRow(`SELECT first_seen, last_mint, credits FROM provision WHERE app=? AND caller=?`, app, caller).
		Scan(&firstSeen, &lastMint, &credits)
	switch {
	case err == nil: // repeat: cooldown check, refresh last_mint, no re-seed
		if cooldown > 0 && now.Sub(time.Unix(lastMint, 0)) < cooldown {
			return ProvisionRecord{}, ErrCooldown
		}
		if _, err := tx.Exec(`UPDATE provision SET last_mint=? WHERE app=? AND caller=?`, now.Unix(), app, caller); err != nil {
			return ProvisionRecord{}, err
		}
		if err := tx.Commit(); err != nil {
			return ProvisionRecord{}, err
		}
		return ProvisionRecord{App: app, Caller: caller, IP: ip, FirstSeen: time.Unix(firstSeen, 0), LastMint: now, Credits: credits, New: false}, nil
	case err == sql.ErrNoRows: // first time: cap check + insert + seed
		if maxPerIP > 0 {
			var distinct int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM provision WHERE app=? AND ip=?`, app, ip).Scan(&distinct); err != nil {
				return ProvisionRecord{}, err
			}
			if distinct >= maxPerIP {
				return ProvisionRecord{}, ErrIPCap
			}
		}
		if _, err := tx.Exec(`INSERT INTO provision (app, caller, ip, first_seen, last_mint, credits) VALUES (?,?,?,?,?,?)`,
			app, caller, ip, now.Unix(), now.Unix(), seed); err != nil {
			return ProvisionRecord{}, err
		}
		if err := tx.Commit(); err != nil {
			return ProvisionRecord{}, err
		}
		return ProvisionRecord{App: app, Caller: caller, IP: ip, FirstSeen: now, LastMint: now, Credits: seed, New: true}, nil
	default:
		return ProvisionRecord{}, err
	}
}

func (s *SQLiteStore) Credit(app, caller string) (int, error) {
	var credits int
	err := s.db.QueryRow(`SELECT credits FROM provision WHERE app=? AND caller=?`, app, caller).Scan(&credits)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return credits, err
}

func (s *SQLiteStore) Debit(app, caller string, n int) (bool, int, error) {
	if n <= 0 {
		c, err := s.Credit(app, caller)
		return true, c, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var credits int
	err = tx.QueryRow(`SELECT credits FROM provision WHERE app=? AND caller=?`, app, caller).Scan(&credits)
	if err == sql.ErrNoRows {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	if credits < n {
		return false, credits, nil
	}
	if _, err := tx.Exec(`UPDATE provision SET credits=credits-? WHERE app=? AND caller=?`, n, app, caller); err != nil {
		return false, credits, err
	}
	if err := tx.Commit(); err != nil {
		return false, credits, err
	}
	return true, credits - n, nil
}

func (s *SQLiteStore) Refund(app, caller string, n int) {
	if n <= 0 {
		return
	}
	_, _ = s.db.Exec(`UPDATE provision SET credits=credits+? WHERE app=? AND caller=?`, n, app, caller)
}

// Snapshot returns all usage cells keyed "app|caller" (for /gw/usage).
func (s *SQLiteStore) Snapshot() map[string]struct {
	Calls int     `json:"calls"`
	Cents float64 `json:"cents"`
} {
	out := map[string]struct {
		Calls int     `json:"calls"`
		Cents float64 `json:"cents"`
	}{}
	rows, err := s.db.Query(`SELECT app, caller, calls, cents FROM usage`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var app, caller string
		var calls int
		var cents float64
		if rows.Scan(&app, &caller, &calls, &cents) == nil {
			out[key(app, caller)] = struct {
				Calls int     `json:"calls"`
				Cents float64 `json:"cents"`
			}{calls, cents}
		}
	}
	return out
}
