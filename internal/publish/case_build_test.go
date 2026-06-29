package publish

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBundle builds a two-platform *Bundle without invoking the cross-compiler,
// so the case-store + artifact-writing logic can be tested fast and offline.
func fakeBundle() *Bundle {
	primary := PlatformBundle{
		Platform:    "linux/amd64",
		Tarball:     []byte("primary-tarball-bytes"),
		TarballName: "io.pilot.async-0.2.0-linux-amd64.tar.gz",
		SHA256:      "1111111111111111111111111111111111111111111111111111111111111111",
	}
	mac := PlatformBundle{
		Platform:    "darwin/arm64",
		Tarball:     []byte("darwin-tarball-bytes"),
		TarballName: "io.pilot.async-0.2.0-darwin-arm64.tar.gz",
		SHA256:      "2222222222222222222222222222222222222222222222222222222222222222",
	}
	return &Bundle{
		Tarball:      primary.Tarball,
		TarballName:  primary.TarballName,
		SHA256:       primary.SHA256,
		Namespace:    "async",
		MetadataJSON: []byte(`{"id":"io.pilot.async","version":"0.2.0"}`),
		Platforms:    []PlatformBundle{primary, mac},
	}
}

// TestCreateSubmittedThenAttachBundle exercises the async-build lifecycle that
// the website form + /admin/build use: record a case with no bundle, then attach
// the built artifacts and flip it to pending.
func TestCreateSubmittedThenAttachBundle(t *testing.T) {
	store, err := NewCaseStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sub := Submission{
		ID: "io.pilot.async", Version: "0.2.0", Description: "async demo",
		Backend: SubBackend{BaseURL: "https://api.async.com"},
	}

	c, err := store.CreateSubmitted(sub)
	if err != nil {
		t.Fatalf("CreateSubmitted: %v", err)
	}
	if c.Status != StatusSubmitted {
		t.Fatalf("status = %q, want submitted", c.Status)
	}
	// No bundle should exist yet.
	if _, err := os.Stat(filepath.Join(store.Dir(c.CaseID), "submission.json")); err == nil {
		t.Error("no submission.json should exist before the build")
	}

	build := BuildInfo{BundleName: "io.pilot.async-0.2.0-linux-amd64.tar.gz", BundleSHA256: "1111111111111111111111111111111111111111111111111111111111111111", Publisher: "ed25519:xyz", BundleBytes: 21, InstalledBytes: 9}
	c2, err := store.AttachBundle(c.CaseID, fakeBundle(), build)
	if err != nil {
		t.Fatalf("AttachBundle: %v", err)
	}
	if c2.Status != StatusPending {
		t.Fatalf("status after attach = %q, want pending", c2.Status)
	}
	if len(c2.History) != 2 || c2.History[1].Note != "bundle built" {
		t.Fatalf("history not appended on attach: %+v", c2.History)
	}

	// Both platform tarballs + metadata.json + submission.json must be on disk.
	d := store.Dir(c.CaseID)
	for _, f := range []string{
		"io.pilot.async-0.2.0-linux-amd64.tar.gz",
		"io.pilot.async-0.2.0-darwin-arm64.tar.gz",
		"metadata.json",
		"submission.json",
	} {
		if _, err := os.Stat(filepath.Join(d, f)); err != nil {
			t.Errorf("expected %s on disk: %v", f, err)
		}
	}

	// submission.json must carry the full os/arch → {file,sha256} bundles map.
	raw, err := os.ReadFile(filepath.Join(d, "submission.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ID      string                       `json:"id"`
		Bundle  string                       `json:"bundle"`
		Bundles map[string]map[string]string `json:"bundles"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("submission.json parse: %v", err)
	}
	if got.ID != "io.pilot.async" {
		t.Errorf("submission.json id = %q", got.ID)
	}
	if len(got.Bundles) != 2 {
		t.Errorf("bundles map has %d platforms, want 2: %+v", len(got.Bundles), got.Bundles)
	}
	if got.Bundles["darwin/arm64"]["file"] != "io.pilot.async-0.2.0-darwin-arm64.tar.gz" {
		t.Errorf("darwin bundle entry wrong: %+v", got.Bundles["darwin/arm64"])
	}
}

func TestCreateSubmittedRefusesApprovedClobber(t *testing.T) {
	store, err := NewCaseStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sub := Submission{ID: "io.pilot.async", Version: "0.2.0", Backend: SubBackend{BaseURL: "https://x.io"}}
	c, err := store.CreateSubmitted(sub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetStatus(c.CaseID, StatusApproved, "shipped"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSubmitted(sub); err == nil {
		t.Error("CreateSubmitted should refuse an already-approved id+version")
	}
}

// --- mailer ---------------------------------------------------------------

func TestMailerDevModeNoKey(t *testing.T) {
	t.Setenv("SENDGRID_API_KEY", "")
	m := NewMailer()
	if m.Enabled() {
		t.Error("mailer should be disabled with no API key")
	}
	// Dev mode logs instead of sending and never errors.
	if err := m.Send("dev@example.com", "subject", "<p>hi</p>", "hi"); err != nil {
		t.Errorf("dev-mode Send should not error, got %v", err)
	}
}

func TestMailerEnabledWithKey(t *testing.T) {
	t.Setenv("SENDGRID_API_KEY", "SG.fake-key")
	t.Setenv("MAIL_FROM", "noreply@pilot.test")
	t.Setenv("MAIL_REGION", "eu")
	m := NewMailer()
	if !m.Enabled() {
		t.Error("mailer should be enabled when a key is set")
	}
	if m.from != "noreply@pilot.test" {
		t.Errorf("from = %q, want override", m.from)
	}
	if m.region != "eu" {
		t.Errorf("region = %q, want eu", m.region)
	}
}

// TestRejectEmailEscapesHTML is a correctness/security check: the rejection
// reason is publisher-influenced text rendered into HTML, so it must be escaped.
func TestRejectEmailEscapesHTML(t *testing.T) {
	sub := Submission{ID: "io.pilot.x", Listing: SubListing{DisplayName: "X"}}
	_, htmlBody, _ := RejectEmail(sub, `<script>alert('xss')</script>`)
	if strings.Contains(htmlBody, "<script>alert") {
		t.Error("reject email must HTML-escape the reason (XSS)")
	}
	if !strings.Contains(htmlBody, "&lt;script&gt;") {
		t.Error("expected escaped <script> entity in reject email")
	}
}

// --- publish.go filesystem helpers ----------------------------------------

func TestCopyDirSkipsDirsAndStatus(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	mustWrite(t, filepath.Join(src, "bundle.tar.gz"), "bundle")
	mustWrite(t, filepath.Join(src, "submission.json"), "{}")
	mustWrite(t, filepath.Join(src, "status.json"), "internal") // must be skipped
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := copyDir(src, dst); err != nil {
		t.Fatalf("copyDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "bundle.tar.gz")); err != nil {
		t.Error("bundle.tar.gz should be copied")
	}
	if _, err := os.Stat(filepath.Join(dst, "submission.json")); err != nil {
		t.Error("submission.json should be copied")
	}
	if _, err := os.Stat(filepath.Join(dst, "status.json")); err == nil {
		t.Error("status.json is server-internal and must NOT be copied")
	}
	if _, err := os.Stat(filepath.Join(dst, "nested")); err == nil {
		t.Error("subdirectories must NOT be copied (flat copy)")
	}
}

func TestCopyDirMissingSource(t *testing.T) {
	if err := copyDir(filepath.Join(t.TempDir(), "absent"), t.TempDir()); err == nil {
		t.Error("copyDir should error when source is missing")
	}
}

func TestGitHelperRunsInDir(t *testing.T) {
	dir := t.TempDir()
	if _, err := git(dir, "init"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	out, err := git(dir, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	if strings.TrimSpace(out) != "true" {
		t.Errorf("expected to be inside a work tree, got %q", out)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- sign.go trimSpace ----------------------------------------------------

func TestTrimSpace(t *testing.T) {
	cases := map[string]string{
		"abc\n":     "abc",
		"abc \r\n ": "abc",
		"abc":       "abc",
		"":          "",
	}
	for in, want := range cases {
		if got := string(trimSpace([]byte(in))); got != want {
			t.Errorf("trimSpace(%q) = %q, want %q", in, got, want)
		}
	}
}
