package publish

import (
	"net/url"
	"testing"

	"github.com/pilot-protocol/app-store/pkg/manifest"
)

func TestFormToConfig(t *testing.T) {
	v := url.Values{
		"id":               {"io.pilot.example"},
		"app_version":      {"0.1.0"},
		"description":      {"Search API"},
		"backend_base_url": {"https://api.example.com"},
		"header_name":      {"x-api-key"},
		"header_value":     {"${PARALLEL_API_KEY}"},
		"method_name":      {"example.search", ""},
		"method_verb":      {"POST", "GET"},
		"method_path":      {"/v1/search", "/skip"},
		"method_duration":  {"med", "fast"},
		"display_name":     {"Example"},
		"license":          {"Apache-2.0"},
		"categories":       {"search, research"},
	}
	cfg, meta := FormToConfig(v)
	if errs := cfg.Validate(); len(errs) > 0 {
		t.Fatalf("expected valid config, got %v", errs)
	}
	if meta.ID != "io.pilot.example" || meta.Version != "0.1.0" {
		t.Errorf("meta wrong: %+v", meta)
	}
	if cfg.Backend.Headers["x-api-key"] != "${PARALLEL_API_KEY}" || !cfg.Backend.NeedsSecrets() {
		t.Errorf("auth header not mapped: %+v", cfg.Backend.Headers)
	}
	if len(cfg.Methods) != 1 || cfg.Methods[0].Name != "example.search" || cfg.Methods[0].HTTP.Verb != "POST" {
		t.Errorf("methods mismapped (empty rows must be skipped): %+v", cfg.Methods)
	}
	if cfg.Listing.DisplayName != "Example" || cfg.Listing.License != "Apache-2.0" || len(cfg.Listing.Categories) != 2 {
		t.Errorf("listing not mapped: %+v", cfg.Listing)
	}
}

func TestSignManifestVerifiesAndDetectsTamper(t *testing.T) {
	dir := t.TempDir()
	priv, err := LoadOrCreateKey(dir + "/k.key")
	if err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{
		ID:              "io.pilot.x",
		AppVersion:      "0.1.0",
		ManifestVersion: 1,
		Binary:          manifest.Binary{Runtime: "go", Path: "bin/x", SHA256: "abc123"},
		Grants:          []manifest.Grant{{Cap: "net.dial", Target: "api.example.com"}},
	}
	if err := SignManifest(m, priv); err != nil {
		t.Fatalf("sign+self-verify failed: %v", err)
	}
	if err := m.VerifySignature(); err != nil {
		t.Fatalf("signature should verify: %v", err)
	}
	// Tampering with a signed field must break verification.
	m.Binary.SHA256 = "def456"
	if err := m.VerifySignature(); err == nil {
		t.Error("tampered binary sha should fail verification")
	}
}

func TestLoadOrCreateKeyIsStable(t *testing.T) {
	p := t.TempDir() + "/k.key"
	a, err := LoadOrCreateKey(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateKey(p) // second load must return the same key
	if err != nil {
		t.Fatal(err)
	}
	if PublisherString(a) != PublisherString(b) {
		t.Error("key not stable across loads")
	}
}
