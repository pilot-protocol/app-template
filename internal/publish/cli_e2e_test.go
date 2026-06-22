package publish

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// sampleCLISubmission is a CLI-backed submission with both method shapes:
// an enumerated subcommand (status) and a passthrough that fronts the whole
// tool (exec) — the "translate all CLI commands" surface.
func sampleCLISubmission() Submission {
	return Submission{
		ID: "io.pilot.gh", Version: "0.1.0", Description: "Fronts the gh CLI over the app store.", Email: "dev@acme.example",
		Backend: SubBackend{Type: "cli", Command: []string{"gh"}, EnvPassthrough: []string{"GH_TOKEN"}},
		Methods: []SubMethod{
			{Name: "gh.status", Description: "Show gh auth status.", Latency: "fast",
				CLI: SubCLIRoute{Args: []string{"auth", "status"}}},
			{Name: "gh.exec", Description: "Run any gh subcommand.", Latency: "med",
				Params: []SubParam{{Name: "args", Type: "array", Description: "verbatim argv forwarded to gh"}},
				CLI:    SubCLIRoute{Passthrough: true}},
		},
		Listing: SubListing{DisplayName: "GitHub CLI", License: "MIT", Categories: []string{"dev"}},
		Vendor:  SubVendor{Name: "Acme", AgentUsage: "agents drive gh", Capabilities: "github"},
	}
}

// fileFromTarball pulls one path out of a gzipped bundle tarball.
func fileFromTarball(t *testing.T, tarball []byte, name string) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == name {
			b, _ := io.ReadAll(tr)
			return b
		}
	}
	t.Fatalf("%s not found in tarball", name)
	return nil
}

// TestCLISubmissionValidates pins the submission-level CLI rules: a well-formed
// cli submission passes, and the common misconfigurations are rejected with a
// friendly message (fast, no build).
func TestCLISubmissionValidates(t *testing.T) {
	t.Parallel()
	if errs := sampleCLISubmission().Validate(); len(errs) != 0 {
		t.Fatalf("a well-formed cli submission must validate, got: %v", errs)
	}

	noCmd := sampleCLISubmission()
	noCmd.Backend.Command = nil
	if !hasSubErr(noCmd.Validate(), "CLI backend requires a command") {
		t.Errorf("missing command should be rejected, got: %v", noCmd.Validate())
	}

	badPass := sampleCLISubmission()
	badPass.Methods[1].CLI.Args = []string{"oops"} // passthrough + args is contradictory
	if !hasSubErr(badPass.Validate(), "passthrough takes argv") {
		t.Errorf("passthrough+args should be rejected, got: %v", badPass.Validate())
	}

	emptyRoute := sampleCLISubmission()
	emptyRoute.Methods[0].CLI = SubCLIRoute{} // no args, no flags, no passthrough
	if !hasSubErr(emptyRoute.Validate(), "needs args, params_as_flags, or passthrough") {
		t.Errorf("empty cli route should be rejected, got: %v", emptyRoute.Validate())
	}
}

// TestCLIHelpPreviewShowsPassthroughArgs verifies the live preview renders the
// correct pilotctl line for a passthrough method: an {"args":[...]} payload, not
// a params skeleton.
func TestCLIHelpPreviewShowsPassthroughArgs(t *testing.T) {
	t.Parallel()
	_, cmds := sampleCLISubmission().HelpPreview()
	joined := strings.Join(cmds, "\n")
	if !strings.Contains(joined, `call io.pilot.gh gh.exec '{"args":[`) {
		t.Errorf("passthrough method should preview an args[] payload; got:\n%s", joined)
	}
	if !strings.Contains(joined, "call io.pilot.gh gh.status") {
		t.Errorf("enumerated method missing from preview:\n%s", joined)
	}
}

// TestCLISubmissionBuildsAndVerifies is the publish-path e2e: a CLI submission
// builds through the real pipeline (scaffold → cross-compile → sign → catalogue
// self-verify) for every platform. Because BuildBundle self-verifies through the
// exact catalogue gate, a successful build PROVES the proc.exec manifest passes
// validation — the gate that rejected cli apps before this capability landed.
// It then asserts the shipped manifest declares proc.exec (scoped to the command)
// and is guarded.
func TestCLISubmissionBuildsAndVerifies(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiles the cli adapter for all platforms; skipped under -short")
	}
	priv, err := LoadOrCreateKey(t.TempDir() + "/k.key")
	if err != nil {
		t.Fatal(err)
	}
	sub := sampleCLISubmission()
	if errs := sub.Validate(); len(errs) != 0 {
		t.Fatalf("submission invalid: %v", errs)
	}

	b, err := BuildBundle(sub.ToConfig(), priv)
	if err != nil {
		t.Fatalf("BuildBundle (implies catalogue self-verify) failed for a proc.exec app: %v", err)
	}
	if len(b.Platforms) != len(DefaultPlatforms) {
		t.Fatalf("want %d platforms, got %d", len(DefaultPlatforms), len(b.Platforms))
	}

	// The shipped manifest must carry the hardened proc.exec grant + guarded.
	mfRaw := fileFromTarball(t, b.Primary().Tarball, "./manifest.json")
	var mf struct {
		Protection string `json:"protection"`
		Grants     []struct {
			Cap, Target string
		} `json:"grants"`
	}
	if err := json.Unmarshal(mfRaw, &mf); err != nil {
		t.Fatalf("parse shipped manifest: %v", err)
	}
	if mf.Protection != "guarded" {
		t.Errorf("cli app must ship protection=guarded, got %q", mf.Protection)
	}
	var procExec string
	for _, g := range mf.Grants {
		if g.Cap == "proc.exec" {
			procExec = g.Target
		}
	}
	if procExec != "gh" {
		t.Errorf("manifest must declare proc.exec scoped to the command (target=gh), got %q", procExec)
	}
}

func hasSubErr(errs []string, substr string) bool {
	for _, e := range errs {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}
