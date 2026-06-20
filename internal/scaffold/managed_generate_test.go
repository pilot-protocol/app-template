package scaffold

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const managedSpec = `
id: io.pilot.sixtyfour
app_version: 0.1.0
description: "Enrichment via the managed Sixtyfour key."
backend:
  type: http
  base_url: https://api.sixtyfour.ai
  auth: managed
methods:
  - name: sixtyfour.enrich
    summary: "Enrich a person/company."
    duration: slow
    http: {verb: POST, path: /enrich}
  - name: sixtyfour.find-email
    summary: "Find an email."
    duration: med
    http: {verb: GET, path: /find-email}
    params: {name: "string", domain: "string"}
`

// A managed app must generate a keyless adapter that points at the broker, is
// granted key.sign, carries no secret, and signs requests the broker can verify.
func TestManagedGeneratesKeylessSigningAdapter(t *testing.T) {
	cfg := parseSpec(t, managedSpec)
	dir := t.TempDir()
	written, err := Generate(cfg, dir)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// Every generated Go file must parse.
	var sawSigner bool
	for _, w := range written {
		if !strings.HasSuffix(w, ".go") {
			continue
		}
		if strings.HasSuffix(w, "signer.go") {
			sawSigner = true
		}
		if _, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, w), nil, parser.AllErrors); err != nil {
			t.Errorf("%s: not valid Go: %v", w, err)
		}
	}
	if !sawSigner {
		t.Fatal("managed app must generate internal/backend/signer.go")
	}

	mf := readFile(t, dir, "manifest.json")
	mustContain(t, "manifest", mf,
		"broker.pilotprotocol.network", // net.dial targets the broker, not the partner
		`"key.sign"`,                   // granted to sign caller identity
		"sixtyfour.enrich",
		"sixtyfour.help",
	)
	mustNotContain(t, "manifest", mf,
		"api.sixtyfour.ai", // partner host must NOT be a dial target on user hosts
		"secrets.json",     // keyless: no per-user secret
	)

	main := readFile(t, dir, filepath.Join("cmd", cfg.BinaryName, "main.go"))
	mustContain(t, "main.go", main,
		"backend.NewSigner", // signer is built from --identity
		"broker.pilotprotocol.network/io.pilot.sixtyfour", // default backend URL is the broker
	)

	// The generated signer's canonical string MUST match the broker verifier
	// (internal/broker/identity.go). Lock the exact expression here so the two
	// ends can't silently drift.
	signer := readFile(t, dir, filepath.Join("internal", "backend", "signer.go"))
	mustContain(t, "signer.go", signer,
		`method + "\n" + path + "\n" + ts + "\n" + base64.RawStdEncoding.EncodeToString(sum[:])`,
		"X-Pilot-Caller",
		"X-Pilot-Signature",
	)
}

func readFile(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func mustContain(t *testing.T, what, body string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(body, s) {
			t.Errorf("%s missing %q", what, s)
		}
	}
}

func mustNotContain(t *testing.T, what, body string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if strings.Contains(body, s) {
			t.Errorf("%s should not contain %q", what, s)
		}
	}
}
