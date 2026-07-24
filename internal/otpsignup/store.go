package otpsignup

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

// record is one caller's minted provider account. Password + APIKey are
// encrypted at rest; Email is stored in the clear (it is a synthetic address we
// minted, not sensitive on its own).
type record struct {
	Email     string
	Password  string
	APIKey    string
	CreatedAt time.Time
}

// store is the per-caller ledger. Secrets are sealed with AES-256-GCM so a DB
// leak does not expose keys/passwords without the separate encryption key.
type store struct {
	db   *sql.DB
	aead cipher.AEAD
}

func openStore(dbPath, encKeyHex string) (*store, error) {
	if dbPath == "" {
		dbPath = ":memory:"
	}
	// A 32-byte key seals secrets at rest. Prod MUST set a stable key: a missing
	// key used to fall back SILENTLY to a fresh random one, which is worse than
	// it looks — every restart would then seal under a NEW key, so store.get()
	// fails to decrypt every row written under the old one. get() treats that as
	// "not found" (see below), so Signup thinks the caller has no account and
	// mints (and pays for) a fresh one, whose INSERT is then silently a no-op
	// (INSERT OR IGNORE against the still-present old row) — every restart from
	// then on re-mints again, forever, and no single key is ever reliably
	// retrievable. That silently defeats the whole idempotency guarantee this
	// store exists for. So: require an explicit key for any persistent store,
	// and only auto-generate one for a genuinely ephemeral (:memory:, e.g. test)
	// store where there is nothing on disk to survive a restart anyway.
	var key []byte
	switch {
	case encKeyHex != "":
		k, err := hex.DecodeString(encKeyHex)
		if err != nil || len(k) != 32 {
			return nil, fmt.Errorf("otpsignup: EncKeyHex must be 64 hex chars (32 bytes)")
		}
		key = k
	case dbPath == ":memory:":
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("otpsignup: generating ephemeral enc key: %w", err)
		}
	default:
		return nil, fmt.Errorf("otpsignup: OTPSIGNUP_ENC_KEY is required for a persistent ledger (%s) — an unset key silently rotates on every restart and corrupts existing accounts (see store.go)", dbPath)
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
	db.SetMaxOpenConns(1) // sqlite: serialize writers
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS accounts (
		caller TEXT PRIMARY KEY,
		email TEXT NOT NULL,
		password_enc TEXT NOT NULL,
		apikey_enc TEXT NOT NULL,
		created_at INTEGER NOT NULL
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

func (s *store) open(enc string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(raw) < s.aead.NonceSize() {
		return "", fmt.Errorf("bad ciphertext")
	}
	nonce, ct := raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():]
	pt, err := s.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

func (s *store) put(caller string, r record) error {
	pw, err := s.seal(r.Password)
	if err != nil {
		return err
	}
	ak, err := s.seal(r.APIKey)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT OR IGNORE INTO accounts (caller,email,password_enc,apikey_enc,created_at) VALUES (?,?,?,?,?)`,
		caller, r.Email, pw, ak, r.CreatedAt.Unix())
	return err
}

func (s *store) get(caller string) (record, bool, error) {
	var email, pwEnc, akEnc string
	var ts int64
	err := s.db.QueryRow(`SELECT email,password_enc,apikey_enc,created_at FROM accounts WHERE caller=?`, caller).
		Scan(&email, &pwEnc, &akEnc, &ts)
	if err == sql.ErrNoRows {
		return record{}, false, nil
	}
	if err != nil {
		return record{}, false, err
	}
	pw, err := s.open(pwEnc)
	if err != nil {
		return record{}, false, err
	}
	ak, err := s.open(akEnc)
	if err != nil {
		return record{}, false, err
	}
	return record{Email: email, Password: pw, APIKey: ak, CreatedAt: time.Unix(ts, 0).UTC()}, true, nil
}
