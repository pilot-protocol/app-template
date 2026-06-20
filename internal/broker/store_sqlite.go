package broker

import (
	"database/sql"
	"fmt"

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
