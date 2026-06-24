package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"io"
	"testing"

	"github.com/pilot-protocol/app-template/internal/publish"
)

// cliArtifactsSubmission is the miren-shaped fixture: a cli backend (id
// io.pilot.<x>) whose binary is DELIVERED from the R2 artifact registry — the
// artifacts[] step with per-platform os/arch/url/sha256/unpack/exec_path. This
// is exactly the shape whose absence on main produced broken published bundles.
func cliArtifactsSubmission() publish.Submission {
	return publish.Submission{
		ID:          "io.pilot.miren",
		Version:     "0.1.0",
		Description: "Delivers and fronts the miren CLI from the registry.",
		Email:       "ops@pilotprotocol.network",
		Backend: publish.SubBackend{
			Type:    "cli",
			Command: []string{"miren"},
		},
		Methods: []publish.SubMethod{
			{Name: "miren.version", Description: "Print the miren version.", Latency: "fast",
				CLI: publish.SubCLIRoute{Args: []string{"version"}}},
			{Name: "miren.exec", Description: "Run any miren subcommand.", Latency: "med",
				Params: []publish.SubParam{{Name: "args", Type: "array"}},
				CLI:    publish.SubCLIRoute{Passthrough: true}},
		},
		Listing: publish.SubListing{DisplayName: "Miren", License: "MIT", Categories: []string{"dev"}, AppDescription: "Miren on Pilot."},
		Vendor:  publish.SubVendor{Name: "Pilot", AgentUsage: "agents drive miren", Capabilities: "microvm"},
		Artifacts: []publish.SubArtifact{
			{OS: "darwin", Arch: "arm64", URL: "https://pub-x.r2.dev/io.pilot.miren/0.1.0/darwin-arm64/miren.tar.gz",
				SHA256: "1111111111111111111111111111111111111111111111111111111111111111",
				Unpack: "tar.gz", ExecPath: "miren-0.1.0-darwin-arm64/miren", Order: 1},
			{OS: "linux", Arch: "amd64", URL: "https://pub-x.r2.dev/io.pilot.miren/0.1.0/linux-amd64/miren",
				SHA256:   "2222222222222222222222222222222222222222222222222222222222222222",
				ExecPath: "bin/miren", Order: 1},
		},
	}
}

// TestVerifyStagingGate_BuildsWiredBundle proves the positive path: a cli
// submission WITH artifacts builds a bundle that actually contains install.json
// and the StageAssets-wired adapter (manifest fs.write $APP grant), so
// checkStaging passes on every platform.
func TestVerifyStagingGate_BuildsWiredBundle(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiles the adapter for all platforms; skipped under -short")
	}
	sub := cliArtifactsSubmission()
	if errs := sub.Validate(); len(errs) != 0 {
		t.Fatalf("fixture must validate, got: %v", errs)
	}
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := publish.BuildBundle(sub.ToConfig(), priv)
	if err != nil {
		t.Fatalf("BuildBundle: %v", err)
	}
	if len(b.Platforms) == 0 {
		t.Fatal("no platforms built")
	}
	for _, p := range b.Platforms {
		msg, ok := checkStaging(p.Tarball)
		if !ok {
			t.Errorf("platform %s: staging check must pass for an artifact-delivering app, got: %s", p.Platform, msg)
		}
	}
}

// TestVerifyStagingGate_FailsWhenStagingStripped is the regression guard: if a
// build produced platform bundles WITHOUT install.json (the exact silent
// breakage that let broken bundles publish), checkStaging — and therefore
// verify-submission — must FAIL. We simulate a stripped bundle by rebuilding the
// tarball without install.json and without the fs.write $APP grant.
func TestVerifyStagingGate_FailsWhenStagingStripped(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiles the adapter; skipped under -short")
	}
	sub := cliArtifactsSubmission()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := publish.BuildBundle(sub.ToConfig(), priv)
	if err != nil {
		t.Fatalf("BuildBundle: %v", err)
	}
	stripped := stripStaging(t, b.Primary().Tarball)
	if msg, ok := checkStaging(stripped); ok {
		t.Fatalf("staging check MUST fail for a bundle missing install.json, but it passed: %s", msg)
	}
}

// stripStaging rewrites a bundle tarball dropping install.json/install.sh and
// the manifest's fs.write $APP grant — modelling a build that declared artifacts
// but silently failed to wire native delivery.
func stripStaging(t *testing.T, tarball []byte) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	var buf bytes.Buffer
	outGz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(outGz)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name := hdr.Name
		if name == "./install.json" || name == "install.json" ||
			name == "./install.sh" || name == "install.sh" {
			continue // drop the staging spec
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if name == "./manifest.json" || name == "manifest.json" {
			body = bytes.ReplaceAll(body,
				[]byte(`{"cap": "fs.write", "target": "$APP"},`), nil)
		}
		hdr.Size = int64(len(body))
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := outGz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
