package publish

import "testing"

// smolSubmission is a provisioned (io.pilot.smol-shaped) submission for the tests.
func smolSubmission() Submission {
	return Submission{
		ID:      "io.pilot.smol",
		Version: "1.2.0",
		Backend: SubBackend{
			Type:               "hybrid",
			Auth:               "provisioned",
			Provider:           "master",
			Command:            []string{"smolvm"},
			CloudBaseURL:       "https://api.smolmachines.com",
			BrokerURL:          "https://smol-broker.pilotprotocol.network",
			SeedCredits:        5,
			CostCredits:        map[string]int{"/push": 1},
			MaxIdentitiesPerIP: 5,
			OwnerEnvKey:        "PILOT_OWNER",
			ArtifactMaxBytes:   268435456,
		},
		Methods: []SubMethod{
			{Name: "smol.exec", Latency: "med", CLI: SubCLIRoute{Passthrough: true}},
			{Name: "smol.push", Latency: "slow", HTTP: SubRoute{Verb: "POST", Path: "/push"}},
			{Name: "smol.provision", Latency: "fast", HTTP: SubRoute{Verb: "POST", Path: "/_provision"}},
		},
	}
}

func TestProvisionedBrokerEntry(t *testing.T) {
	s := smolSubmission()

	if got := s.MasterKeyEnv(); got != "SMOL_MASTER_KEY" {
		t.Fatalf("MasterKeyEnv=%s", got)
	}
	if got := s.DeriveSecretEnv(); got != "SMOL_DERIVE_SECRET" {
		t.Fatalf("DeriveSecretEnv=%s", got)
	}
	if got := s.AdminKeyEnv(); got != "SMOL_ADMIN_KEY" {
		t.Fatalf("AdminKeyEnv=%s", got)
	}

	e := s.BrokerEntry()
	if e.ID != "io.pilot.smol" || e.KeyEnv != "SMOL_MASTER_KEY" {
		t.Fatalf("entry id/keyenv: %+v", e)
	}
	if e.Upstream != "https://api.smolmachines.com" {
		t.Fatalf("upstream=%s", e.Upstream)
	}
	if e.Provision == nil {
		t.Fatal("Provision must be set for a provisioned submission")
	}
	p := e.Provision
	if p.Provider != "master" || p.SecretEnv != "SMOL_DERIVE_SECRET" {
		t.Fatalf("provider/secret: %+v", p)
	}
	if p.SeedCredits != 5 || p.CostCredits["/push"] != 1 || p.MaxIdentitiesPerIP != 5 {
		t.Fatalf("credit/cap knobs: %+v", p)
	}
	if p.OwnerEnvKey != "PILOT_OWNER" || p.ArtifactMaxBytes != 268435456 {
		t.Fatalf("owner/artifact: %+v", p)
	}
	// Allow lists ONLY the http (cloud) method paths — the local cli method is
	// never brokered.
	if len(e.Allow) != 2 {
		t.Fatalf("allow should hold the 2 http paths, got %v", e.Allow)
	}
	seen := map[string]bool{}
	for _, a := range e.Allow {
		seen[a] = true
	}
	if !seen["/push"] || !seen["/_provision"] {
		t.Fatalf("allow missing http paths: %v", e.Allow)
	}
}

func TestProvisionedToConfigHybrid(t *testing.T) {
	cfg := smolSubmission().ToConfig()
	if cfg.Backend.Type != "hybrid" {
		t.Fatalf("backend type=%s", cfg.Backend.Type)
	}
	if cfg.Backend.BrokerURL != "https://smol-broker.pilotprotocol.network" {
		t.Fatalf("broker_url not threaded: %q", cfg.Backend.BrokerURL)
	}
	if !cfg.Provisioned() || !cfg.IsHybrid() {
		t.Fatal("cfg should be provisioned + hybrid")
	}
}

func TestTokenMintAdminKeyEnvDefault(t *testing.T) {
	s := smolSubmission()
	s.Backend.Provider = "tokenmint"
	s.Backend.AdminKeyEnv = "" // unset ⇒ derive the default from the id
	e := s.BrokerEntry()
	if e.Provision == nil || e.Provision.AdminKeyEnv != "SMOL_ADMIN_KEY" {
		t.Fatalf("tokenmint AdminKeyEnv should default to SMOL_ADMIN_KEY: %+v", e.Provision)
	}
}
