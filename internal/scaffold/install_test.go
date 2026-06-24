package scaffold

import (
	"encoding/json"
	"strings"
	"testing"
)

// depSpec: three assets on one platform where deps force an order that differs
// from the raw `order` field, proving the topological resolver (not just the
// integer order) drives the install sequence.
//
//	runtime (order 9, no deps)
//	plugin  (order 1, deps: [runtime])   -> must come AFTER runtime despite lower order
//	tool    (order 5, deps: [plugin])    -> must come last
const depSpec = `
id: io.pilot.toolx
app_version: 0.3.0
description: "Multi-artifact app with dependencies."
backend:
  type: cli
  command: ["tool"]
assets:
  - {name: plugin,  os: darwin, arch: arm64, url: "https://r.example/plugin",  sha256: "1111111111111111111111111111111111111111111111111111111111111111", exec_path: bin/plugin,  order: 1, deps: [runtime]}
  - {name: tool,    os: darwin, arch: arm64, url: "https://r.example/tool",    sha256: "2222222222222222222222222222222222222222222222222222222222222222", exec_path: bin/tool,    order: 5, deps: [plugin], args: ["--init"]}
  - {name: runtime, os: darwin, arch: arm64, url: "https://r.example/runtime", sha256: "3333333333333333333333333333333333333333333333333333333333333333", exec_path: bin/runtime, order: 9}
methods:
  - {name: toolx.run, summary: "run", cli: {passthrough: true}}
`

func TestDependencyInstallOrder(t *testing.T) {
	cfg := parseSpec(t, depSpec)
	if errs := cfg.Validate(); len(errs) != 0 {
		t.Fatalf("valid dep spec must pass: %v", errs)
	}
	seq, err := cfg.ResolveAssets("darwin", "arm64")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := []string{seq[0].AssetName(), seq[1].AssetName(), seq[2].AssetName()}
	want := []string{"runtime", "plugin", "tool"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("install order = %v, want %v (deps must override raw order)", got, want)
		}
	}
}

func TestInstallSpecAndScriptHonorDeps(t *testing.T) {
	cfg := parseSpec(t, depSpec)

	// install.json: resolved Order is the topo index, not the raw order field.
	raw, err := marshalInstallSpec(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var spec InstallSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for _, a := range spec.Assets {
		pos[a.Name] = a.Order
	}
	if !(pos["runtime"] < pos["plugin"] && pos["plugin"] < pos["tool"]) {
		t.Fatalf("install.json order wrong: %+v", pos)
	}

	// install.sh: the staged lines must appear in dependency order, and the
	// tool's install arg must be emitted after it stages.
	sh, err := renderInstallScript(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := string(sh)
	ir := strings.Index(s, "https://r.example/runtime")
	ip := strings.Index(s, "https://r.example/plugin")
	it := strings.Index(s, "https://r.example/tool")
	if !(ir >= 0 && ir < ip && ip < it) {
		t.Fatalf("install.sh stage order wrong (runtime=%d plugin=%d tool=%d)", ir, ip, it)
	}
	if !strings.Contains(s, `"$APP/bin/tool" '--init'`) {
		t.Errorf("install.sh missing the tool's install-arg invocation:\n%s", s)
	}
	if !strings.HasPrefix(s, "#!/usr/bin/env sh") {
		t.Errorf("install.sh missing shebang")
	}
}

// parseNoValidate parses + resolves but does NOT fail on validation errors, so
// negative cases can assert the error themselves.
func parseNoValidate(t *testing.T, spec string) *Config {
	t.Helper()
	cfg, err := Parse([]byte(spec))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg.Resolve()
	return cfg
}

func TestDependencyCycleRejected(t *testing.T) {
	cyc := strings.Replace(depSpec, "exec_path: bin/runtime, order: 9}", "exec_path: bin/runtime, order: 9, deps: [tool]}", 1)
	errs := parseNoValidate(t, cyc).Validate()
	if !anyContains(errs, "cycle") {
		t.Fatalf("a dependency cycle must be rejected, got: %v", errs)
	}
}

func TestUnknownDepRejected(t *testing.T) {
	bad := strings.Replace(depSpec, "deps: [plugin]", "deps: [nope]", 1)
	errs := parseNoValidate(t, bad).Validate()
	if !anyContains(errs, "unknown asset") {
		t.Fatalf("an unknown dep must be rejected, got: %v", errs)
	}
}

func anyContains(errs []error, sub string) bool {
	for _, e := range errs {
		if strings.Contains(e.Error(), sub) {
			return true
		}
	}
	return false
}
