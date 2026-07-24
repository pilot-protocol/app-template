package otpsignup

import (
	"path/filepath"
	"testing"
)

// validEncKeyHex is 64 hex chars (32 bytes) — a well-formed at-rest key for tests.
const validEncKeyHex = "abababababababababababababababababababababababababababababababab"

// TestOpenStoreRequiresEncKeyForPersistentDB is the fix for the silent
// ephemeral-key fallback: a persistent (non-:memory:) ledger with no configured
// key must fail loudly at startup, not quietly generate a throwaway key that
// makes every previously-sealed row undecryptable after the next restart (see
// the comment on openStore for why that silently breaks idempotency).
func TestOpenStoreRequiresEncKeyForPersistentDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "accounts.db")
	if _, err := openStore(dbPath, ""); err == nil {
		t.Fatal("expected openStore to fail without an enc key for a persistent db path")
	}
	// A valid 32-byte hex key must still work for a persistent path.
	st, err := openStore(dbPath, validEncKeyHex)
	if err != nil {
		t.Fatalf("openStore with a valid key: %v", err)
	}
	if st == nil || st.db == nil {
		t.Fatal("expected a usable store")
	}
}

// TestOpenStoreAllowsEphemeralKeyForInMemoryDB confirms the exception is
// scoped precisely: a genuinely ephemeral (:memory:, or the default when
// DBPath is unset) store may still auto-generate a key, since there is nothing
// on disk that a lost key could ever fail to decrypt after a restart.
func TestOpenStoreAllowsEphemeralKeyForInMemoryDB(t *testing.T) {
	if _, err := openStore(":memory:", ""); err != nil {
		t.Fatalf("in-memory store should work without an explicit enc key: %v", err)
	}
	if _, err := openStore("", ""); err != nil {
		t.Fatalf("empty dbPath (defaults to :memory:) should work without an explicit enc key: %v", err)
	}
}

// TestOpenStoreRejectsMalformedEncKey keeps the existing validation intact
// alongside the new persistent-path requirement.
func TestOpenStoreRejectsMalformedEncKey(t *testing.T) {
	if _, err := openStore(":memory:", "not-hex"); err == nil {
		t.Fatal("expected openStore to reject a non-hex enc key")
	}
	if _, err := openStore(":memory:", "abcd"); err == nil {
		t.Fatal("expected openStore to reject a too-short (non-32-byte) enc key")
	}
}
