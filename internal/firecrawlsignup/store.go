package firecrawlsignup

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo)
)

// record is one caller's provisioned Firecrawl account. APIKey is encrypted at
// rest; the mailbox address and the ToS acceptance are stored in the clear —
// the acceptance in particular MUST be readable without the encryption key,
// because it is the artefact we would show Firecrawl in an audit.
type record struct {
	Email      string
	APIKey     string
	TermsURL   string
	TermsRev   string
	AcceptedAt time.Time
	CreatedAt  time.Time
}

// store is the per-caller ledger. The Firecrawl key is sealed with AES-256-GCM
// so a DB leak does not expose usable keys without the separate encryption key.
type store struct {
	db   *sql.DB
	aead cipher.AEAD
}

func openStore(dbPath, encKeyHex string) (*store, error) {
	if dbPath == "" {
		dbPath = ":memory:"
	}
	// A 32-byte key seals the api key at rest. Prod MUST set a stable key (else a
	// restart cannot decrypt prior rows); tests fall back to an ephemeral one.
	var key []byte
	if encKeyHex != "" {
		k, err := hex.DecodeString(encKeyHex)
		if err != nil || len(k) != 32 {
			return nil, fmt.Errorf("firecrawlsignup: EncKeyHex must be 64 hex chars (32 bytes)")
		}
		key = k
	} else {
		key = make([]byte, 32)
		_, _ = rand.Read(key)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS accounts (
		caller      TEXT PRIMARY KEY,
		email       TEXT NOT NULL,
		api_key     TEXT NOT NULL,
		terms_url   TEXT NOT NULL DEFAULT '',
		terms_rev   TEXT NOT NULL DEFAULT '',
		accepted_at INTEGER NOT NULL DEFAULT 0,
		created_at  INTEGER NOT NULL
	)`); err != nil {
		return nil, err
	}
	return &store{db: db, aead: aead}, nil
}

func (s *store) seal(plain string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := s.aead.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

func (s *store) open(sealed string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", err
	}
	if len(raw) < s.aead.NonceSize() {
		return "", fmt.Errorf("firecrawlsignup: sealed value too short")
	}
	nonce, ct := raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():]
	plain, err := s.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// put records a caller's account. It is INSERT-OR-IGNORE so two concurrent
// signups for the same caller resolve to exactly one account — the same
// first-writer-wins shape the credit ledger uses.
func (s *store) put(caller string, r record) error {
	sealed, err := s.seal(r.APIKey)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT OR IGNORE INTO accounts (caller, email, api_key, terms_url, terms_rev, accepted_at, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		caller, r.Email, sealed, r.TermsURL, r.TermsRev, r.AcceptedAt.Unix(), r.CreatedAt.Unix())
	return err
}

func (s *store) get(caller string) (record, bool, error) {
	var (
		r                 record
		sealed            string
		acceptedAt, creAt int64
	)
	err := s.db.QueryRow(
		`SELECT email, api_key, terms_url, terms_rev, accepted_at, created_at FROM accounts WHERE caller = ?`,
		caller).Scan(&r.Email, &sealed, &r.TermsURL, &r.TermsRev, &acceptedAt, &creAt)
	if err == sql.ErrNoRows {
		return record{}, false, nil
	}
	if err != nil {
		return record{}, false, err
	}
	key, err := s.open(sealed)
	if err != nil {
		return record{}, false, err
	}
	r.APIKey = key
	r.AcceptedAt = time.Unix(acceptedAt, 0).UTC()
	r.CreatedAt = time.Unix(creAt, 0).UTC()
	return r, true, nil
}

// replace swaps a caller's stored key (used after a rotate).
func (s *store) replace(caller, apiKey string) error {
	sealed, err := s.seal(apiKey)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE accounts SET api_key = ? WHERE caller = ?`, sealed, caller)
	return err
}
