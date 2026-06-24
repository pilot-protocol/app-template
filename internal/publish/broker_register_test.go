package publish

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pilot-protocol/app-template/internal/broker"
)

func managedSub() Submission {
	return Submission{
		ID:      "io.pilot.partner",
		Version: "0.1.0",
		Backend: SubBackend{
			BaseURL: "https://api.example.com/",
			Auth:    "managed",
			Quota:   100,
			Headers: []SubHeader{{Name: "x-api-key", Value: "managed"}},
		},
		Methods: []SubMethod{
			{Name: "partner.enrich", HTTP: SubRoute{Verb: "POST", Path: "/enrich"}},
			{Name: "partner.find-email", HTTP: SubRoute{Verb: "GET", Path: "/find-email"}},
		},
	}
}

func TestBrokerEntryDerivedFromSubmission(t *testing.T) {
	e := managedSub().BrokerEntry()
	if e.ID != "io.pilot.partner" {
		t.Errorf("id = %q", e.ID)
	}
	if e.Upstream != "https://api.example.com" { // trailing slash trimmed
		t.Errorf("upstream = %q", e.Upstream)
	}
	if e.KeyEnv != "PARTNER_MASTER_KEY" {
		t.Errorf("key_env = %q, want PARTNER_MASTER_KEY", e.KeyEnv)
	}
	if e.AuthHeader != "x-api-key" {
		t.Errorf("auth_header = %q, want x-api-key", e.AuthHeader)
	}
	if len(e.Allow) != 2 || e.Allow[0] != "/enrich" || e.Allow[1] != "/find-email" {
		t.Errorf("allow = %v, want [/enrich /find-email]", e.Allow)
	}
	if e.Quota != 100 {
		t.Errorf("quota = %d, want 100 (set at publish time)", e.Quota)
	}
}

func TestManagedToConfigIsKeyless(t *testing.T) {
	cfg := managedSub().ToConfig()
	if !cfg.Managed() {
		t.Fatal("managed submission must produce a managed config")
	}
	if len(cfg.Backend.Headers) != 0 {
		t.Fatalf("managed adapter must be keyless, got headers %v", cfg.Backend.Headers)
	}
}

func TestFileRegistrarUpsertsIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry", "apps.json")
	reg := FileRegistrar{Path: path}

	if err := reg.Register(managedSub().BrokerEntry()); err != nil {
		t.Fatal(err)
	}
	// Re-register (e.g. re-approval) must update in place, not duplicate.
	updated := managedSub().BrokerEntry()
	updated.Quota = 5
	if err := reg.Register(updated); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := broker.ParseRegistry(b, func(string) string { return "TESTKEY" })
	if err != nil {
		t.Fatalf("broker could not load the written registry: %v", err)
	}
	app := entries.Get("io.pilot.partner")
	if app == nil {
		t.Fatal("registry missing the app after registration")
	}
	if app.Quota != 5 {
		t.Fatalf("re-registration did not update quota: got %d, want 5", app.Quota)
	}
	// Confirm exactly one entry (no duplicate) by re-reading raw.
	raw, _ := readEntries(path)
	if len(raw) != 1 {
		t.Fatalf("expected 1 entry after idempotent upsert, got %d", len(raw))
	}
}

// A Bearer API: the header value "Bearer managed" makes BrokerEntry derive
// auth_scheme "Bearer" (so the broker injects "Authorization: Bearer <key>"),
// and templated method paths flow through to the allow-list verbatim.
func TestBrokerEntryBearerScheme(t *testing.T) {
	sub := Submission{
		ID:      "io.pilot.agentphone",
		Version: "0.1.0",
		Backend: SubBackend{
			BaseURL: "https://api.agentphone.ai",
			Auth:    "managed",
			Headers: []SubHeader{{Name: "Authorization", Value: "Bearer managed"}},
		},
		Methods: []SubMethod{
			{Name: "agentphone.place_call", HTTP: SubRoute{Verb: "POST", Path: "/v1/calls"}},
			{Name: "agentphone.get_call", HTTP: SubRoute{Verb: "GET", Path: "/v1/calls/{call_id}"}},
		},
	}
	e := sub.BrokerEntry()
	if e.KeyEnv != "AGENTPHONE_MASTER_KEY" {
		t.Errorf("key_env = %q, want AGENTPHONE_MASTER_KEY", e.KeyEnv)
	}
	if e.AuthHeader != "Authorization" || e.AuthScheme != "Bearer" {
		t.Errorf("auth = %q/%q, want Authorization/Bearer", e.AuthHeader, e.AuthScheme)
	}
	if len(e.Allow) != 2 || e.Allow[1] != "/v1/calls/{call_id}" {
		t.Errorf("allow = %v, want the templated path preserved", e.Allow)
	}
}
