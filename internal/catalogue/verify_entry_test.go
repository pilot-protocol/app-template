package catalogue

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/pilot-protocol/app-store/pkg/manifest"
)

// --- fixtures -------------------------------------------------------------

// signManifest mirrors publish.SignManifest's wire format exactly. It is kept
// local so the catalogue package (a leaf) does not import the publish
// orchestrator just to build a test fixture. Format (see manifest.signingPayload):
// publisher:id:manifest_version:binary.sha256:grants-sha256-hex.
func signManifest(t *testing.T, m *manifest.Manifest, priv ed25519.PrivateKey) {
	t.Helper()
	m.Store.Publisher = "ed25519:" + base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	grantsJSON, err := json.Marshal(m.Grants)
	if err != nil {
		t.Fatalf("marshal grants: %v", err)
	}
	grantsHash := sha256.Sum256(grantsJSON)
	payload := fmt.Sprintf("%s:%s:%d:%s:%x",
		m.Store.Publisher, m.ID, m.ManifestVersion, m.Binary.SHA256, grantsHash)
	m.Store.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(payload)))
	if err := m.VerifySignature(); err != nil {
		t.Fatalf("fixture self-verify failed: %v", err)
	}
}

// validManifest returns a schema-valid manifest (passes manifest.Validate()
// once binary.sha256 is set and it is signed).
func validManifest() *manifest.Manifest {
	return &manifest.Manifest{
		ID:              "io.pilot.example",
		AppVersion:      "1.0.0",
		ManifestVersion: 1,
		Binary:          manifest.Binary{Runtime: "go", Path: "bin/example-app"},
		Exposes:         []string{"example.help", "example.do"},
		Grants:          []manifest.Grant{{Cap: "audit.log", Target: "events"}},
	}
}

type bundleOpts struct {
	mutate       func(*manifest.Manifest) // applied before signing
	tamperBinary bool                     // write a different binary than the pinned sha
	omitHelp     bool                     // drop the *.help method from exposes
}

// buildBundle constructs a real signed .tar.gz and returns its bytes plus the
// (post-signing) manifest.
func buildBundle(t *testing.T, opts bundleOpts) ([]byte, *manifest.Manifest) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	bin := []byte("\x7fELF fake-binary-bytes-for-example-app")
	sum := sha256.Sum256(bin)

	m := validManifest()
	m.Binary.SHA256 = hex.EncodeToString(sum[:])
	if opts.omitHelp {
		m.Exposes = []string{"example.do"}
	}
	if opts.mutate != nil {
		opts.mutate(m)
	}
	signManifest(t, m, priv)

	mfRaw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	writeBin := bin
	if opts.tamperBinary {
		writeBin = append([]byte("TAMPERED-"), bin...)
	}
	raw := tarGz(t, map[string][]byte{"manifest.json": mfRaw, "bin/example-app": writeBin})
	return raw, m
}

func tarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: "./" + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}
	return buf.Bytes()
}

// writeBundle writes raw to a temp file and returns a file:// URL.
func writeBundle(t *testing.T, raw []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	return "file://" + p
}

// entryFor builds a correct catalogue Entry for a bundle (right sha, id, version).
func entryFor(t *testing.T, raw []byte, m *manifest.Manifest) Entry {
	t.Helper()
	sum := sha256.Sum256(raw)
	return Entry{
		ID:           m.ID,
		Version:      m.AppVersion,
		Description:  "example app",
		BundleURL:    writeBundle(t, raw),
		BundleSHA256: hex.EncodeToString(sum[:]),
	}
}

func findCheck(t *testing.T, r Result, name string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in result (%d checks)", name, len(r.Checks))
	return Check{}
}

func mustFail(t *testing.T, r Result, name string) {
	t.Helper()
	if c := findCheck(t, r, name); c.OK {
		t.Errorf("check %q should have FAILED but passed: %s", name, c.Msg)
	}
}

func mustPass(t *testing.T, r Result, name string) {
	t.Helper()
	if c := findCheck(t, r, name); !c.OK {
		t.Errorf("check %q should have PASSED but failed: %s", name, c.Msg)
	}
}

// --- the happy path -------------------------------------------------------

func TestVerifyEntry_HappyPath(t *testing.T) {
	raw, m := buildBundle(t, bundleOpts{})
	e := entryFor(t, raw, m)

	r := VerifyEntry(e, nil)
	if !r.OK() {
		for _, c := range r.Checks {
			if !c.OK {
				t.Errorf("unexpected failing check %q: %s", c.Name, c.Msg)
			}
		}
		t.Fatalf("a valid signed bundle must pass every gate check")
	}
	// Spot-check that the security-critical checks were actually run.
	mustPass(t, r, "signature verifies")
	mustPass(t, r, "binary.sha256 pin matches binary")
	mustPass(t, r, "manifest Validate()")
	mustPass(t, r, "exposes a <ns>.help method")
}

// --- each gate rejects the right tampering --------------------------------

func TestVerifyEntry_RejectsBundleSHAMismatch(t *testing.T) {
	raw, m := buildBundle(t, bundleOpts{})
	e := entryFor(t, raw, m)
	e.BundleSHA256 = "deadbeef" // catalogue lies about the tarball hash

	r := VerifyEntry(e, nil)
	mustFail(t, r, "bundle_sha256 matches")
	if r.OK() {
		t.Error("result must be not-OK on sha mismatch")
	}
}

func TestVerifyEntry_RejectsBinaryShaMismatch(t *testing.T) {
	// The bundle's binary differs from the sha pinned (and signed) in the manifest.
	raw, m := buildBundle(t, bundleOpts{tamperBinary: true})
	e := entryFor(t, raw, m)

	r := VerifyEntry(e, nil)
	mustFail(t, r, "binary.sha256 pin matches binary")
}

func TestVerifyEntry_RejectsForgedSignature(t *testing.T) {
	_, m := buildBundle(t, bundleOpts{})
	// Re-pack with a corrupted signature.
	m.Store.Signature = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	mfRaw, _ := json.Marshal(m)
	bin := []byte("\x7fELF fake-binary-bytes-for-example-app")
	raw := tarGz(t, map[string][]byte{"manifest.json": mfRaw, "bin/example-app": bin})
	e := entryFor(t, raw, m)

	r := VerifyEntry(e, nil)
	mustFail(t, r, "signature verifies")
}

func TestVerifyEntry_RejectsIDMismatch(t *testing.T) {
	raw, m := buildBundle(t, bundleOpts{})
	e := entryFor(t, raw, m)
	e.ID = "io.pilot.imposter" // catalogue id disagrees with the signed manifest

	r := VerifyEntry(e, nil)
	mustFail(t, r, "catalogue.id == manifest.id")
}

func TestVerifyEntry_RejectsVersionMismatch(t *testing.T) {
	raw, m := buildBundle(t, bundleOpts{})
	e := entryFor(t, raw, m)
	e.Version = "9.9.9"

	r := VerifyEntry(e, nil)
	mustFail(t, r, "catalogue.version == manifest.app_version")
}

func TestVerifyEntry_RejectsMissingHelp(t *testing.T) {
	raw, m := buildBundle(t, bundleOpts{omitHelp: true})
	e := entryFor(t, raw, m)

	r := VerifyEntry(e, nil)
	mustFail(t, r, "exposes a <ns>.help method")
}

func TestVerifyEntry_RejectsInvalidManifestSchema(t *testing.T) {
	// A bad runtime makes manifest.Validate() fail (but the bundle still signs,
	// since runtime is not part of the signing payload).
	raw, m := buildBundle(t, bundleOpts{mutate: func(m *manifest.Manifest) {
		m.Binary.Runtime = "rust" // not in KnownRuntimes
	}})
	e := entryFor(t, raw, m)

	r := VerifyEntry(e, nil)
	mustFail(t, r, "manifest Validate()")
}

func TestVerifyEntry_RejectsDowngrade(t *testing.T) {
	raw, m := buildBundle(t, bundleOpts{}) // 1.0.0
	e := entryFor(t, raw, m)
	prev := Entry{ID: m.ID, Version: "2.0.0"}

	r := VerifyEntry(e, &prev)
	mustFail(t, r, "not a version downgrade")
}

func TestVerifyEntry_AllowsSameVersionReplace(t *testing.T) {
	raw, m := buildBundle(t, bundleOpts{}) // 1.0.0
	e := entryFor(t, raw, m)
	prev := Entry{ID: m.ID, Version: "1.0.0"}

	r := VerifyEntry(e, &prev)
	mustPass(t, r, "not a version downgrade")
}

func TestVerifyEntry_FetchFailureShortCircuits(t *testing.T) {
	e := Entry{
		ID:           "io.pilot.example",
		Version:      "1.0.0",
		BundleURL:    "file:///nonexistent/path/bundle.tar.gz",
		BundleSHA256: "deadbeef",
	}
	r := VerifyEntry(e, nil)
	mustFail(t, r, "bundle_url resolves")
	if r.OK() {
		t.Error("unreachable bundle must be not-OK")
	}
	// It must short-circuit: only the fetch check ran.
	if len(r.Checks) != 1 {
		t.Errorf("expected exactly 1 check after fetch failure, got %d", len(r.Checks))
	}
}

// --- EntryForBundle / ReadBundleFacts round-trips -------------------------

func TestEntryForBundleRoundTrips(t *testing.T) {
	raw, m := buildBundle(t, bundleOpts{})
	p := filepath.Join(t.TempDir(), "io.pilot.example-1.0.0.tar.gz")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	e, err := EntryForBundle(p)
	if err != nil {
		t.Fatalf("EntryForBundle: %v", err)
	}
	if e.ID != m.ID || e.Version != m.AppVersion {
		t.Errorf("got id=%q version=%q, want %q/%q", e.ID, e.Version, m.ID, m.AppVersion)
	}
	sum := sha256.Sum256(raw)
	if e.BundleSHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha mismatch: %s", e.BundleSHA256)
	}
	// The entry it builds should itself pass verification.
	if r := VerifyEntry(e, nil); !r.OK() {
		for _, c := range r.Checks {
			if !c.OK {
				t.Errorf("EntryForBundle output failed check %q: %s", c.Name, c.Msg)
			}
		}
	}
}

func TestReadBundleFacts(t *testing.T) {
	raw, m := buildBundle(t, bundleOpts{})
	p := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := ReadBundleFacts(p)
	if err != nil {
		t.Fatalf("ReadBundleFacts: %v", err)
	}
	if f.ID != m.ID || f.Version != m.AppVersion {
		t.Errorf("facts id/version = %q/%q", f.ID, f.Version)
	}
	if f.Publisher != m.Store.Publisher {
		t.Errorf("publisher = %q, want %q", f.Publisher, m.Store.Publisher)
	}
	if f.BundleBytes != int64(len(raw)) {
		t.Errorf("BundleBytes = %d, want %d", f.BundleBytes, len(raw))
	}
	if f.InstalledBytes <= 0 {
		t.Errorf("InstalledBytes should be > 0, got %d", f.InstalledBytes)
	}
}

// --- VerifyCatalogue ------------------------------------------------------

func writeCatalogue(t *testing.T, entries ...Entry) string {
	t.Helper()
	c := Catalogue{Version: 2, UpdatedAt: "2026-01-01T00:00:00Z", Apps: entries}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "catalogue.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVerifyCatalogue_AllValid(t *testing.T) {
	raw, m := buildBundle(t, bundleOpts{})
	e := entryFor(t, raw, m)
	path := writeCatalogue(t, e)

	results, err := VerifyCatalogue(path, "")
	if err != nil {
		t.Fatalf("VerifyCatalogue: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].OK() {
		t.Errorf("valid catalogue entry should pass")
	}
}

func TestVerifyCatalogue_DowngradeAgainstOld(t *testing.T) {
	raw, m := buildBundle(t, bundleOpts{}) // 1.0.0
	e := entryFor(t, raw, m)
	newPath := writeCatalogue(t, e)
	// Old catalogue had the same app at a HIGHER version.
	oldPath := writeCatalogue(t, Entry{ID: m.ID, Version: "2.0.0", BundleURL: "file:///ignored", BundleSHA256: "x"})

	results, err := VerifyCatalogue(newPath, oldPath)
	if err != nil {
		t.Fatalf("VerifyCatalogue: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	mustFail(t, results[0], "not a version downgrade")
}

func TestVerifyCatalogue_MissingFile(t *testing.T) {
	if _, err := VerifyCatalogue("/nonexistent/catalogue.json", ""); err == nil {
		t.Error("expected error for missing catalogue file")
	}
}

// --- regression: prerelease downgrade guard (compareSemver) ---------------

func TestCompareSemverPrerelease(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0-rc1", "1.0.0", -1},           // prerelease ranks below the release
		{"1.0.0", "1.0.0-rc1", 1},            // and the release above the prerelease
		{"1.0.0-rc1", "1.0.0-rc2", -1},       // rc1 < rc2
		{"1.0.0-rc2", "1.0.0-rc1", 1},        //
		{"1.0.0-rc1", "1.0.0-rc1", 0},        // identical
		{"1.0.0-alpha", "1.0.0-alpha.1", -1}, // fewer identifiers ranks lower
		{"1.0.0-1", "1.0.0-alpha", -1},       // numeric ranks below alphanumeric
		{"2.0.0", "1.0.0-rc1", 1},            // numeric version dominates
	}
	for _, c := range cases {
		if got := compareSemver(c.a, c.b); got != c.want {
			t.Errorf("compareSemver(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestVerifyEntry_RejectsPrereleaseDowngrade(t *testing.T) {
	// Republishing a prerelease over an existing released version is a downgrade.
	raw, m := buildBundle(t, bundleOpts{mutate: func(m *manifest.Manifest) {
		m.AppVersion = "1.0.0-rc1"
	}})
	e := entryFor(t, raw, m)
	prev := Entry{ID: m.ID, Version: "1.0.0"}

	r := VerifyEntry(e, &prev)
	mustFail(t, r, "not a version downgrade")
}

// --- regression: extractBundle decompression cap (zip-bomb) ---------------

func TestExtractBundleRejectsTooManyEntries(t *testing.T) {
	files := map[string][]byte{
		"manifest.json": []byte(`{"binary":{"path":"bin/app"}}`),
		"bin/app":       []byte("x"),
	}
	for i := 0; i < maxBundleEntries+8; i++ {
		files[fmt.Sprintf("pad/%d.dat", i)] = []byte("p")
	}
	raw := tarGz(t, files)

	if _, _, err := extractBundle(raw); err == nil {
		t.Error("expected rejection of a bundle exceeding the entry-count cap")
	}
}
