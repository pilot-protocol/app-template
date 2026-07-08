package insforgesignup

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

// record is one caller's provisioned InsForge project. APIKey (the project
// access key) is encrypted at rest; the project/org ids and the backend URL are
// stored in the clear (opaque identifiers, not sensitive on their own).
type record struct {
	ProjectID  string
	APIKey     string
	BackendURL string
	OrgID      string
	CreatedAt  time.Time
}

// store is the per-caller ledger. The project key is sealed with AES-256-GCM so
// a DB leak does not expose keys without the separate encryption key.
type store struct {
	db   *sql.DB
	aead cipher.AEAD
}

func openStore(dbPath, encKeyHex string) (*store, error) {
	if dbPath == "" {
		dbPath = ":memory:"
	}
	// A 32-byte key seals the project key at rest. Prod MUST set a stable key
	// (else a restart can't decrypt prior rows); tests fall back to an ephemeral one.
	var key []byte
	if encKeyHex != "" {
		k, err := hex.DecodeString(encKeyHex)
		if err != nil || len(k) != 32 {
			return nil, fmt.Errorf("insforgesignup: EncKeyHex must be 64 hex chars (32 bytes)")
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
	db.SetMaxOpenConns(1) // sqlite: serialize writers
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS projects (
		caller TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		apikey_enc TEXT NOT NULL,
		backend_url TEXT NOT NULL,
		org_id TEXT NOT NULL,
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
	ak, err := s.seal(r.APIKey)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT OR IGNORE INTO projects (caller,project_id,apikey_enc,backend_url,org_id,created_at) VALUES (?,?,?,?,?,?)`,
		caller, r.ProjectID, ak, r.BackendURL, r.OrgID, r.CreatedAt.Unix())
	return err
}

func (s *store) get(caller string) (record, bool, error) {
	var projectID, akEnc, backendURL, orgID string
	var ts int64
	err := s.db.QueryRow(`SELECT project_id,apikey_enc,backend_url,org_id,created_at FROM projects WHERE caller=?`, caller).
		Scan(&projectID, &akEnc, &backendURL, &orgID, &ts)
	if err == sql.ErrNoRows {
		return record{}, false, nil
	}
	if err != nil {
		return record{}, false, err
	}
	ak, err := s.open(akEnc)
	if err != nil {
		return record{}, false, err
	}
	return record{ProjectID: projectID, APIKey: ak, BackendURL: backendURL, OrgID: orgID, CreatedAt: time.Unix(ts, 0).UTC()}, true, nil
}
