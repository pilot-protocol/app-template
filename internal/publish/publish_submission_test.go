package publish

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestPublishSubmissionKeepsTheStoreListing runs scripts/publish-submission.sh
// on a pointer submission, as publish-on-merge does (relative dir, from the
// repo root), with gh and git push faked. The script looked for metadata.json
// by that relative path after cd'ing into the platform clone, never found it,
// and published the bare entry: wallet 0.4.0's catalogue PR lost its display
// name, vendor, categories, license, source and metadata page. The PR text's
// platform list was a jq syntax error.
func TestPublishSubmissionKeepsTheStoreListing(t *testing.T) {
	for _, tool := range []string{"bash", "jq", "git", "tar", "shasum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	if exec.Command("bash", "-c", "type mapfile").Run() != nil {
		t.Skip("bash has no mapfile (bash 3); the script needs bash 4+, as on CI")
	}
	realGit, _ := exec.LookPath("git")

	const id, version = "io.pilot.metax", "0.2.0"
	const publisher = "ed25519:TESTONLY"
	root := t.TempDir()

	// The submission, under the app-template checkout the script runs from.
	repo := filepath.Join(root, "app-template")
	dir := filepath.Join(repo, "submissions", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bundle := fakeRichBundle(t, `{"store":{"publisher":"`+publisher+`"}}`)
	bundles := map[string]any{}
	for _, plat := range []string{"linux/amd64", "darwin/arm64"} {
		file := id + "-" + version + "-" + strings.ReplaceAll(plat, "/", "-") + ".tar.gz"
		if err := os.WriteFile(filepath.Join(dir, file), bundle, 0o644); err != nil {
			t.Fatal(err)
		}
		bundles[plat] = map[string]string{"file": file, "sha256": sha256Hex(bundle)}
	}
	writeJSON(t, filepath.Join(dir, "submission.json"), map[string]any{
		"id": id, "version": version, "namespace": "metax", "description": "A pointer app.",
		"bundle": id + "-" + version + "-linux-amd64.tar.gz", "bundle_sha256": sha256Hex(bundle),
		"bundles": bundles,
	})
	writeJSON(t, filepath.Join(dir, "metadata.json"), map[string]any{
		"id": id, "display_name": "MetaX", "vendor": map[string]any{"name": "Pilot Protocol"},
		"license": "AGPL-3.0-or-later", "source_url": "https://github.com/pilot-protocol/metax",
		"categories": []string{"payments", "crypto"},
	})

	// The platform repo the fake gh clones.
	platform := filepath.Join(root, "platform")
	if err := os.MkdirAll(filepath.Join(platform, "catalogue"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(platform, "catalogue", "catalogue.json"), map[string]any{"version": 2, "apps": []any{}})
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "base"}} {
		if out, err := exec.Command(realGit, append([]string{"-C", platform}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Fakes: gh clones the fixture and records the PR; git push copies the
	// branch as pushed; pilot-app passes every gate.
	out := filepath.Join(root, "out")
	bin := filepath.Join(root, "bin")
	for _, d := range []string{out, bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExec(t, filepath.Join(bin, "gh"), `#!/usr/bin/env bash
case "$1 $2" in
  "release view") exit 1 ;;
  "release create"|"release upload") exit 0 ;;
  "repo clone") exec "$REAL_GIT" clone -q "$FIXTURE_PLATFORM" "$4" ;;
  "pr create") printf '%s\n' "$@" > "$OUT/pr-args"; exit 0 ;;
esac
echo "fake gh: unexpected $*" >&2; exit 2
`)
	writeExec(t, filepath.Join(bin, "git"), `#!/usr/bin/env bash
if [ "$1" = push ]; then cp -R . "$OUT/pushed"; exit 0; fi
exec "$REAL_GIT" "$@"
`)
	writeExec(t, filepath.Join(bin, "pilot-app"), "#!/bin/sh\nexit 0\n")

	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "publish-submission.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script, "submissions/"+id)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"REAL_GIT="+realGit, "FIXTURE_PLATFORM="+platform, "OUT="+out,
		"GH_TOKEN=test", "CATALOG_SIGN_KEY=",
		"PILOT_APP_BIN="+filepath.Join(bin, "pilot-app"),
	)
	logs, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("publish-submission.sh: %v\n%s", err, logs)
	}
	if strings.Contains(string(logs), "jq: error") {
		t.Errorf("the script hit a jq error:\n%s", logs)
	}

	pushed := filepath.Join(out, "pushed")
	var cat struct {
		Apps []map[string]any `json:"apps"`
	}
	readJSON(t, filepath.Join(pushed, "catalogue", "catalogue.json"), &cat)
	if len(cat.Apps) != 1 {
		t.Fatalf("catalogue apps = %v, want the one entry", cat.Apps)
	}
	entry := cat.Apps[0]
	md, err := os.ReadFile(filepath.Join(pushed, "catalogue", "apps", id, "metadata.json"))
	if err != nil {
		t.Fatalf("metadata.json was not published with the entry: %v\n%s", err, logs)
	}
	want := map[string]any{
		"display_name":    "MetaX",
		"vendor":          "Pilot Protocol",
		"license":         "AGPL-3.0-or-later",
		"source_url":      "https://github.com/pilot-protocol/metax",
		"categories":      []any{"payments", "crypto"},
		"metadata_url":    "https://raw.githubusercontent.com/pilot-protocol/pilotprotocol/main/catalogue/apps/" + id + "/metadata.json",
		"metadata_sha256": sha256Hex(md),
		"publisher":       publisher,
	}
	for k, w := range want {
		if got := entry[k]; !reflect.DeepEqual(got, w) {
			t.Errorf("entry.%s = %#v, want %#v", k, got, w)
		}
	}
	if b, _ := entry["bundles"].(map[string]any); len(b) != 2 {
		t.Errorf("entry.bundles = %#v, want both platforms", entry["bundles"])
	}

	pr, err := os.ReadFile(filepath.Join(out, "pr-args"))
	if err != nil {
		t.Fatalf("no PR was opened: %v\n%s", err, logs)
	}
	if !strings.Contains(string(pr), "Platforms: darwin/arm64, linux/amd64.") {
		t.Errorf("PR text does not list the platforms:\n%s", pr)
	}
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
