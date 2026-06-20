package scaffold

import "testing"

func TestManagedBackendIsKeylessAndPointsAtBroker(t *testing.T) {
	byo := &Config{ID: "io.pilot.acme", Backend: Backend{Type: "http", BaseURL: "https://api.acme.com",
		Headers: map[string]string{"x-api-key": "${ACME_KEY}"}}}
	if byo.Managed() {
		t.Fatal("byo app should not be managed")
	}
	if got := byo.AdapterBackendURL(); got != "https://api.acme.com" {
		t.Fatalf("byo adapter URL = %s, want the API directly", got)
	}
	if !byo.Backend.NeedsSecrets() {
		t.Fatal("byo app with ${} header must need a secret")
	}

	managed := &Config{ID: "io.pilot.sixtyfour", Backend: Backend{Type: "http", BaseURL: "https://api.sixtyfour.ai", Auth: "managed"}}
	if !managed.Managed() {
		t.Fatal("auth: managed not detected")
	}
	if got := managed.AdapterBackendURL(); got != "https://broker.pilotprotocol.network/io.pilot.sixtyfour" {
		t.Fatalf("managed adapter URL = %s, want the broker/<id>", got)
	}
	if managed.Backend.NeedsSecrets() {
		t.Fatal("managed app must be keyless (no secret on user hosts)")
	}
}
