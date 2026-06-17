package publish

import (
	"strings"
	"testing"

	"github.com/pilot-protocol/app-store/pkg/manifest"
)

func sampleSubmission() Submission {
	return Submission{
		ID: "io.pilot.weather", Version: "0.1.0", Description: "Weather over a public API.",
		Backend: SubBackend{BaseURL: "https://api.example.com", Headers: []SubHeader{{Name: "x-api-key", Value: "${WEATHER_KEY}"}}},
		Methods: []SubMethod{
			{Name: "weather.current", Description: "Current conditions.", Latency: "fast",
				HTTP: SubRoute{Verb: "GET", Path: "/current"}, Params: []SubParam{{Name: "lat", Type: "string", Required: true}}},
		},
		Listing: SubListing{DisplayName: "Weather", License: "MIT", Categories: []string{"weather"}},
		Vendor:  SubVendor{Name: "Acme", AgentUsage: "agents call current for conditions", Capabilities: "weather lookup"},
	}
}

func TestSubmissionValidateEnforcesIoPilotAndLatency(t *testing.T) {
	if errs := sampleSubmission().Validate(); len(errs) > 0 {
		t.Fatalf("valid submission rejected: %v", errs)
	}
	bad := sampleSubmission()
	bad.ID = "com.acme.weather"
	if errs := bad.Validate(); len(errs) == 0 {
		t.Error("expected io.pilot.* prefix to be required")
	}
	noLat := sampleSubmission()
	noLat.Methods = []SubMethod{{Name: "weather.x", Description: "d", HTTP: SubRoute{Verb: "GET", Path: "/x"}}}
	if errs := noLat.Validate(); len(errs) == 0 {
		t.Error("expected latency to be required")
	}
	noDesc := sampleSubmission()
	noDesc.Methods = []SubMethod{{Name: "weather.x", Latency: "fast", HTTP: SubRoute{Verb: "GET", Path: "/x"}}}
	if errs := noDesc.Validate(); len(errs) == 0 {
		t.Error("expected method description to be required")
	}
}

func TestSubmissionToConfigBuildable(t *testing.T) {
	cfg := sampleSubmission().ToConfig()
	if errs := cfg.Validate(); len(errs) > 0 {
		t.Fatalf("derived config invalid: %v", errs)
	}
	if cfg.Backend.Headers["x-api-key"] != "${WEATHER_KEY}" || !cfg.Backend.NeedsSecrets() {
		t.Error("headers/secrets not carried to config")
	}
	if len(cfg.Methods) != 1 || cfg.Methods[0].Duration != "fast" {
		t.Errorf("method/latency not mapped: %+v", cfg.Methods)
	}
}

func TestHelpPreviewIncludesPilotctlAndHelpMethod(t *testing.T) {
	help, cmds := sampleSubmission().HelpPreview()
	var names []string
	for _, m := range help.Methods {
		names = append(names, m.Method)
	}
	if !contains(names, "weather.current") || !contains(names, "weather.help") {
		t.Errorf("help missing methods: %v", names)
	}
	joined := strings.Join(cmds, "\n")
	for _, want := range []string{"pilotctl appstore install io.pilot.weather", "weather.help", "weather.current"} {
		if !strings.Contains(joined, want) {
			t.Errorf("pilotctl preview missing %q in:\n%s", want, joined)
		}
	}
}

func TestSignManifestVerifiesAndDetectsTamper(t *testing.T) {
	priv, err := LoadOrCreateKey(t.TempDir() + "/k.key")
	if err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{ID: "io.pilot.x", AppVersion: "0.1.0", ManifestVersion: 1,
		Binary: manifest.Binary{Runtime: "go", Path: "bin/x", SHA256: "abc"},
		Grants: []manifest.Grant{{Cap: "net.dial", Target: "api.example.com"}}}
	if err := SignManifest(m, priv); err != nil {
		t.Fatalf("sign: %v", err)
	}
	m.Binary.SHA256 = "def"
	if err := m.VerifySignature(); err == nil {
		t.Error("tampered manifest should fail verification")
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
