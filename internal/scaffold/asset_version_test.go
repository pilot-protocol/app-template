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

// An explicit url: is an escape hatch — a native tool may carry its own version
// (e.g. an adapter at 0.1.0 delivering a CLI at 0.10.0). It is accepted as-is; the
// sha256 is the integrity anchor. Only file: opts into version derivation.
func TestValidateAllowsExplicitURLWithOwnVersion(t *testing.T) {
	c := &Config{
		ID:          "io.pilot.toolx",
		AppVersion:  "0.1.0",
		Description: "test app",
		Backend:     Backend{Type: "cli", Command: []string{"toolx"}},
		Methods:     []Method{{Name: "toolx.run", CLI: &CLIRoute{Passthrough: true}}},
		Assets: []Asset{
			// the delivered tool's own version (0.10.0) differs from the adapter's
			{OS: "linux", Arch: "amd64", ExecPath: "bin/toolx", SHA256: hex64(), Order: 1,
				URL: "https://pub-abc.r2.dev/io.pilot.toolx/0.10.0/linux-amd64/toolx-0.10.0"},
		},
	}
	c.Resolve()
	if errs := c.Validate(); len(errs) != 0 {
		t.Fatalf("explicit url with its own version should validate, got %v", errs)
	}
}

func hex64() string { return "0000000000000000000000000000000000000000000000000000000000000000" }
