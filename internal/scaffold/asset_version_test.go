package scaffold

import "testing"

// A file-only asset derives its URL from app_version, so the registry path's
// version always tracks the adapter version (single source of truth).
func TestResolveDerivesAssetURLFromVersion(t *testing.T) {
	c := &Config{
		ID:          "io.pilot.toolx",
		AppVersion:  "1.2.3",
		Description: "test app",
		Backend:     Backend{Type: "cli", Command: []string{"toolx"}},
		Methods:     []Method{{Name: "toolx.run", CLI: &CLIRoute{Passthrough: true}}},
		Assets: []Asset{
			{OS: "linux", Arch: "amd64", File: "toolx", ExecPath: "bin/toolx", SHA256: hex64(), Order: 1},
		},
	}
	c.Resolve()
	want := "https://artifacts.pilotprotocol.network/io.pilot.toolx/1.2.3/linux-amd64/toolx"
	if got := c.Assets[0].URL; got != want {
		t.Fatalf("derived URL = %q, want %q", got, want)
	}
	if errs := c.Validate(); len(errs) != 0 {
		t.Fatalf("derived-URL spec should validate, got %v", errs)
	}
}

// An explicit registry URL whose version segment != app_version is rejected.
func TestValidateRejectsAssetVersionDrift(t *testing.T) {
	c := &Config{
		ID:          "io.pilot.toolx",
		AppVersion:  "1.2.3",
		Description: "test app",
		Backend:     Backend{Type: "cli", Command: []string{"toolx"}},
		Methods:     []Method{{Name: "toolx.run", CLI: &CLIRoute{Passthrough: true}}},
		Assets: []Asset{
			// stale version 1.2.2 in the path
			{OS: "linux", Arch: "amd64", ExecPath: "bin/toolx", SHA256: hex64(), Order: 1,
				URL: "https://artifacts.pilotprotocol.network/io.pilot.toolx/1.2.2/linux-amd64/toolx"},
		},
	}
	c.Resolve()
	errs := c.Validate()
	if !hasErrContaining(errs, "!= app_version") {
		t.Fatalf("expected an asset-version-drift error, got %v", errs)
	}
}

// A non-registry host is not version-checked (publisher's own CDN may use any layout).
func TestValidateAllowsNonRegistryURL(t *testing.T) {
	c := &Config{
		ID:          "io.pilot.toolx",
		AppVersion:  "1.2.3",
		Description: "test app",
		Backend:     Backend{Type: "cli", Command: []string{"toolx"}},
		Methods:     []Method{{Name: "toolx.run", CLI: &CLIRoute{Passthrough: true}}},
		Assets: []Asset{
			{OS: "linux", Arch: "amd64", ExecPath: "bin/toolx", SHA256: hex64(), Order: 1,
				URL: "https://downloads.example.com/anything/toolx"},
		},
	}
	c.Resolve()
	if errs := c.Validate(); len(errs) != 0 {
		t.Fatalf("non-registry URL should validate, got %v", errs)
	}
}

func TestRegistryURLVersion(t *testing.T) {
	u := "https://artifacts.pilotprotocol.network/io.pilot.toolx/9.9.9/darwin-arm64/toolx.tar.gz"
	if v := RegistryURLVersion(u, "io.pilot.toolx"); v != "9.9.9" {
		t.Fatalf("version = %q, want 9.9.9", v)
	}
	if !IsRegistryURL(u) {
		t.Fatal("artifacts.pilotprotocol.network should be a registry host")
	}
	if !IsRegistryURL("https://pub-abc.r2.dev/x/1.0.0/linux-amd64/x") {
		t.Fatal("*.r2.dev should be a registry host")
	}
	if IsRegistryURL("https://downloads.example.com/x") {
		t.Fatal("example.com should not be a registry host")
	}
}

func hex64() string { return "0000000000000000000000000000000000000000000000000000000000000000" }
