package scaffold

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hybridProvisionedSpec is the io.pilot.smol shape: a hybrid app that runs local
// cli methods (exec/version) AND routes cloud methods (push/provision/balance/
// list) to the broker with the provisioned per-user model, delivering the smolvm
// binary from the R2 registry. It exercises every hybrid code path: the exec
// runner, the cloud client + signer, the stager, and the per-method dispatch.
const hybridProvisionedSpec = `
id: io.pilot.smol
app_version: 0.1.0
description: "Smol Machines — local microVMs plus one-command cloud push."
namespace: smol
backend:
  type: hybrid
  auth: provisioned
  command: ["smolvm"]
assets:
  - {os: darwin, arch: arm64, url: "https://pub-x.r2.dev/io.pilot.smol/0.1.0/darwin-arm64/smolvm", sha256: "1111111111111111111111111111111111111111111111111111111111111111", exec_path: bin/smolvm, order: 1}
  - {os: linux,  arch: amd64, url: "https://pub-x.r2.dev/io.pilot.smol/0.1.0/linux-amd64/smolvm",  sha256: "2222222222222222222222222222222222222222222222222222222222222222", exec_path: bin/smolvm, order: 1}
methods:
  - name: smol.exec
    summary: "Passthrough: any smolvm subcommand (local)."
    duration: med
    cli: {passthrough: true}
  - name: smol.version
    summary: "Print the smolvm version (local)."
    duration: fast
    cli: {args: ["--version"]}
  - name: smol.push
    summary: "Push a local VM to the smol cloud."
    duration: slow
    http: {verb: POST, path: /push}
  - name: smol.provision
    summary: "Provision (or return) your cloud key."
    duration: fast
    http: {verb: POST, path: /_provision}
  - name: smol.balance
    summary: "Your remaining cloud credit."
    duration: fast
    http: {verb: GET, path: /_balance}
  - name: smol.list
    summary: "List your cloud VMs."
    duration: fast
    http: {verb: GET, path: /list}
`

// TestGeneratedHybridProjectCompiles type-checks the hybrid+provisioned code
// paths that no cli-only or http-only compile test reaches: BOTH backends built
// in main, the per-method dispatch (cli→runner, http→cloudHandler), the cloud
// client + signer, and the union manifest grants. A stray unused var/import in
// the hybrid branch parses fine but fails `go build` — exactly what this catches.
func TestGeneratedHybridProjectCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping compile test in -short mode")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not available")
	}

	cfg := parseSpec(t, hybridProvisionedSpec)
	dir := t.TempDir()
	if _, err := Generate(cfg, dir); err != nil {
		t.Fatalf("generate: %v", err)
	}

	// A hybrid+provisioned app emits both backends, the signer, and the stager.
	for _, f := range []string{
		filepath.Join("internal", "backend", "exec.go"),
		filepath.Join("internal", "backend", "cloud.go"),
		filepath.Join("internal", "backend", "signer.go"),
		filepath.Join("internal", "backend", "stage.go"),
	} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("hybrid app must emit %s: %v", f, err)
		}
	}

	// Union grants: local exec, broker dial, key signing, and secrets read+write.
	manifest, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"cap": "proc.exec", "target": "smolvm"`,
		`"cap": "net.dial", "target": "broker.pilotprotocol.network"`,
		`"cap": "key.sign", "target": "self"`,
		`"cap": "fs.read", "target": "$APP/secrets.json"`,
		`"cap": "fs.write", "target": "$APP/secrets.json"`,
		`"protection": "guarded"`,
	} {
		if !strings.Contains(string(manifest), want) {
			t.Errorf("manifest missing union grant %q\n%s", want, manifest)
		}
	}

	if sum, err := os.ReadFile(filepath.Join("..", "..", "go.sum")); err == nil {
		if err := os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o644); err != nil {
			t.Fatalf("seed go.sum: %v", err)
		}
	}
	cmd := exec.Command(goBin, "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated hybrid project failed to compile: %v\n%s", err, out)
	}
}
