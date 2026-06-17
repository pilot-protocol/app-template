// Package publish is the submission server's engine: it turns a web-form app
// definition into a signed, verifiable bundle (reusing the scaffold + catalogue
// packages), stores it for review, and on approval triggers the publish
// workflow. v1 uses ONE platform signing key (no per-user keys).
package publish

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"github.com/pilot-protocol/app-store/pkg/manifest"
)

// LoadOrCreateKey loads the base64 ed25519 private key at path, or generates and
// persists one (0600) if absent. v1: a single platform key signs every app.
func LoadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	if b, err := os.ReadFile(path); err == nil {
		raw, derr := base64.StdEncoding.DecodeString(string(trimSpace(b)))
		if derr != nil {
			return nil, fmt.Errorf("decode key %s: %w", path, derr)
		}
		if len(raw) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("key %s: wrong size %d", path, len(raw))
		}
		return ed25519.PrivateKey(raw), nil
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	enc := base64.StdEncoding.EncodeToString(priv)
	if err := os.WriteFile(path, []byte(enc+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write key %s: %w", path, err)
	}
	return priv, nil
}

// PublisherString returns the "ed25519:<base64>" publisher id for a key.
func PublisherString(priv ed25519.PrivateKey) string {
	pub := priv.Public().(ed25519.PublicKey)
	return "ed25519:" + base64.StdEncoding.EncodeToString(pub)
}

// SignManifest sets store.publisher + store.signature on m using priv, over the
// exact payload pkg/manifest verifies (publisher:id:manifest_version:binary.sha256:grants-hash),
// then self-checks with the platform's own VerifySignature so a payload-format
// mistake fails here rather than at install.
func SignManifest(m *manifest.Manifest, priv ed25519.PrivateKey) error {
	m.Store.Publisher = PublisherString(priv)
	grantsJSON, err := json.Marshal(m.Grants)
	if err != nil {
		return fmt.Errorf("marshal grants: %w", err)
	}
	grantsHash := sha256.Sum256(grantsJSON)
	payload := fmt.Sprintf("%s:%s:%d:%s:%x",
		m.Store.Publisher, m.ID, m.ManifestVersion, m.Binary.SHA256, grantsHash)
	sig := ed25519.Sign(priv, []byte(payload))
	m.Store.Signature = base64.StdEncoding.EncodeToString(sig)
	if err := m.VerifySignature(); err != nil {
		return fmt.Errorf("self-verify after signing: %w", err)
	}
	return nil
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}
