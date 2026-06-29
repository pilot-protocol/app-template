// Command pilot-app scaffolds a complete, buildable Pilot Protocol app-store
// adapter from a declarative pilot.app.yaml spec — the same shape as the
// hand-written reference app io.pilot.cosift, generated in seconds.
//
//	pilot-app init      -c pilot.app.yaml -o ./out   scaffold a project
//	pilot-app validate  -c pilot.app.yaml            check a spec
//	pilot-app example                                print a starter spec
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pilot-protocol/app-template/internal/catalogue"
	"github.com/pilot-protocol/app-template/internal/publish"
	"github.com/pilot-protocol/app-template/internal/scaffold"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "init":
		cmdInit(os.Args[2:])
	case "validate":
		cmdValidate(os.Args[2:])
	case "verify":
		cmdVerify(os.Args[2:])
	case "verify-submission":
		cmdVerifySubmission(os.Args[2:])
	case "verify-update":
		cmdVerifyUpdate(os.Args[2:])
	case "submit":
		cmdSubmit(os.Args[2:])
	case "update":
		cmdUpdate(os.Args[2:])
	case "example":
		fmt.Print(scaffold.ExampleSpec)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "pilot-app: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `pilot-app — scaffold a Pilot Protocol app-store adapter from config

Usage:
  pilot-app init     -c pilot.app.yaml -o <dir>   generate the adapter project
  pilot-app validate -c pilot.app.yaml            validate a spec, no output
  pilot-app verify   <bundle.tar.gz | catalogue.json>
                                                  run the catalogue review-gate checks (SPEC §7.1)
  pilot-app update   -c pilot.app.yaml --bump patch|minor|major [-o <project-dir>]
                                                  bump app_version (single source of truth) + re-scaffold
  pilot-app verify-update <submissions/<id>/submission.json>
                                                  enforce the update gate: same publisher key + higher version
  pilot-app submit   -C <project-dir> --prepare <app-template-fork>
                                                  write a submission PR payload (the single front door)
  pilot-app example                               print a starter pilot.app.yaml

After init:
  cd <dir> && make gen-key && make package && pilot-app submit -C . --prepare <fork>
`)
}

func cmdInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	cfgPath := fs.String("c", "pilot.app.yaml", "path to the spec file")
	outDir := fs.String("o", "", "output dir (default: ./<binary_name>)")
	force := fs.Bool("force", false, "overwrite an existing non-empty output dir")
	_ = fs.Parse(args)

	cfg := loadAndCheck(*cfgPath)

	dir := *outDir
	if dir == "" {
		dir = "./" + cfg.BinaryName
	}
	if !*force {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			fatalf("output dir %s is not empty (use --force to overwrite)", dir)
		}
	}

	written, err := scaffold.Generate(cfg, dir)
	if err != nil {
		fatalf("generate: %v", err)
	}
	fmt.Printf("scaffolded %s (%s backend) into %s:\n", cfg.ID, cfg.Backend.Type, dir)
	for _, w := range written {
		fmt.Printf("  %s\n", filepath.Join(dir, w))
	}

	// Resolve deps now so the scaffold ships a working go.sum — then a bare
	// `go build` (or an IDE) works, not just `make`. Best-effort: if the Go
	// toolchain isn't on PATH, the Makefile's `tidy` step still covers it.
	if out, err := runGoModTidy(dir); err != nil {
		fmt.Printf("\nnote: skipped `go mod tidy` (%v) — run it in %s before building.\n%s", err, dir, out)
	} else {
		fmt.Printf("  go.sum (resolved deps)\n")
	}

	fmt.Printf("\nnext:\n  cd %s\n  make gen-key && make package && pilot-app submit -C . --prepare <fork>\n", dir)
}

// runGoModTidy runs `go mod tidy` in dir to materialize go.sum. Returns the
// command output on failure for diagnostics.
func runGoModTidy(dir string) (string, error) {
	if _, err := exec.LookPath("go"); err != nil {
		return "", fmt.Errorf("go not found on PATH")
	}
	cmd := exec.Command("go", "mod", "tidy")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// cmdUpdate ships a new version of an existing app the easy way: it bumps
// app_version — the SINGLE source of truth — in pilot.app.yaml, so every derived
// artifact (asset URLs, install.json, the catalogue entry) tracks it
// automatically; then, given -o, it RE-SCAFFOLDS the adapter project so any
// changed entrypoints/methods/backend are regenerated. The publisher never edits
// a version in two places. Re-sign with the SAME publisher key (key_file is
// unchanged) via `make package`, then `submit --prepare`.
func cmdUpdate(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	cfgPath := fs.String("c", "pilot.app.yaml", "path to the spec file")
	bump := fs.String("bump", "", "semver part to increment: patch | minor | major")
	setV := fs.String("set-version", "", "set an explicit version (overrides --bump)")
	outDir := fs.String("o", "", "re-scaffold the adapter project into this dir (--force) so entrypoint changes propagate")
	_ = fs.Parse(args)

	raw, err := os.ReadFile(*cfgPath)
	if err != nil {
		fatalf("read %s: %v", *cfgPath, err)
	}
	// Resolve the new version from the current one.
	cur := loadAndCheck(*cfgPath)
	var next string
	switch {
	case *setV != "":
		next = *setV
	case *bump != "":
		next, err = bumpSemver(cur.AppVersion, *bump)
		if err != nil {
			fatalf("%v", err)
		}
	default:
		fatalf("specify --bump patch|minor|major or --set-version x.y.z")
	}
	if !scaffold.IsSemver(next) {
		fatalf("resulting version %q is not semver", next)
	}

	newRaw, ok := rewriteAppVersion(raw, next)
	if !ok {
		fatalf("could not find an `app_version:` line in %s to bump", *cfgPath)
	}
	if err := os.WriteFile(*cfgPath, newRaw, 0o644); err != nil {
		fatalf("write %s: %v", *cfgPath, err)
	}
	fmt.Printf("bumped %s: app_version %s → %s\n", cur.ID, cur.AppVersion, next)

	// Re-validate the edited spec (also re-derives asset URLs onto the new version).
	updated := loadAndCheck(*cfgPath)

	if *outDir != "" {
		written, err := scaffold.Generate(updated, *outDir)
		if err != nil {
			fatalf("re-scaffold: %v", err)
		}
		fmt.Printf("re-scaffolded %s into %s (%d files) — entrypoint/spec changes regenerated\n", updated.ID, *outDir, len(written))
		if out, err := runGoModTidy(*outDir); err != nil {
			fmt.Printf("note: skipped `go mod tidy` (%v) — run it in %s before building.\n%s", err, *outDir, out)
		}
		fmt.Printf("\nnext:\n  cd %s\n  make package          # rebuild every platform, re-sign with your existing key\n  pilot-app submit -C . --prepare <app-template-fork>\n  # then in the fork: git add submissions && git commit && gh pr create\n", *outDir)
	} else {
		fmt.Printf("\nnext:\n  pilot-app init -c %s -o <project-dir> --force   # re-scaffold (or pass -o here)\n  cd <project-dir> && make package && pilot-app submit -C . --prepare <fork>\n", *cfgPath)
	}
	if updated.HasAssets() {
		fmt.Printf("\nassets: this app ships native binaries. Upload the new v%s binaries to the\n  registry (pilot-app artifact upload — see docs/R2-ARTIFACT-REGISTRY.md) so the\n  derived URLs resolve. Their version path tracks app_version automatically.\n", next)
	}
}

// bumpSemver increments the patch/minor/major part of a MAJOR.MINOR.PATCH version
// (dropping any prerelease/build suffix), e.g. bump("1.2.3","minor") = "1.3.0".
func bumpSemver(v, part string) (string, error) {
	core := v
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		core = core[:i]
	}
	ps := strings.Split(core, ".")
	if len(ps) != 3 {
		return "", fmt.Errorf("version %q is not MAJOR.MINOR.PATCH", v)
	}
	maj, mnr, pat := atoiOrZero(ps[0]), atoiOrZero(ps[1]), atoiOrZero(ps[2])
	switch part {
	case "major":
		maj, mnr, pat = maj+1, 0, 0
	case "minor":
		mnr, pat = mnr+1, 0
	case "patch":
		pat++
	default:
		return "", fmt.Errorf("--bump must be patch, minor, or major (got %q)", part)
	}
	return fmt.Sprintf("%d.%d.%d", maj, mnr, pat), nil
}

func atoiOrZero(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// rewriteAppVersion replaces the value on the first top-level `app_version:` line,
// preserving indentation, quoting style, and any trailing comment — a surgical
// edit that keeps the rest of the YAML (comments included) intact.
func rewriteAppVersion(raw []byte, next string) ([]byte, bool) {
	re := regexp.MustCompile(`(?m)^(\s*app_version:\s*)("?)[^\s"#]+("?)(.*)$`)
	if !re.Match(raw) {
		return raw, false
	}
	out := re.ReplaceAll(raw, []byte("${1}${2}"+next+"${3}${4}"))
	return out, true
}

func cmdValidate(args []string) {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	cfgPath := fs.String("c", "pilot.app.yaml", "path to the spec file")
	_ = fs.Parse(args)
	cfg := loadAndCheck(*cfgPath)
	fmt.Printf("ok: %s — %d method(s), %s backend\n", cfg.ID, len(cfg.Methods), cfg.Backend.Type)
}

func cmdVerify(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	_ = fs.Parse(args)
	target := fs.Arg(0)
	if target == "" {
		fatalf("usage: pilot-app verify <bundle.tar.gz | catalogue.json>")
	}

	var results []catalogue.Result
	if strings.HasSuffix(target, ".json") {
		old := os.Getenv("PILOT_CATALOGUE_BASE") // optional prior catalogue, for the downgrade check
		rs, err := catalogue.VerifyCatalogue(target, old)
		if err != nil {
			fatalf("%v", err)
		}
		results = rs
	} else {
		entry, err := catalogue.EntryForBundle(target)
		if err != nil {
			fatalf("read bundle: %v", err)
		}
		results = []catalogue.Result{catalogue.VerifyEntry(entry, nil)}
	}

	allOK := true
	for _, r := range results {
		fmt.Printf("\n%s:\n", r.ID)
		for _, c := range r.Checks {
			mark := "✓"
			if !c.OK {
				mark, allOK = "✗", false
			}
			fmt.Printf("  %s %-34s %s\n", mark, c.Name, c.Msg)
		}
	}
	if !allOK {
		fmt.Fprintln(os.Stderr, "\nVERIFY FAILED — fix the ✗ items before submitting.")
		os.Exit(1)
	}
	fmt.Println("\nVERIFY OK — bundle(s) pass the catalogue review gate.")
}

// cmdVerifySubmission builds every platform from a rich submission.json via the
// same pipeline the publish-api uses, then runs the catalogue review gate on
// each built bundle. This is the PR-flow equivalent of the website/API path:
// the adapter is scaffolded by us (never hand-built) and every platform is
// verified, so a single committed tarball is not required.
func cmdVerifySubmission(args []string) {
	fs := flag.NewFlagSet("verify-submission", flag.ExitOnError)
	_ = fs.Parse(args)
	target := fs.Arg(0)
	if target == "" {
		fatalf("usage: pilot-app verify-submission <submission.json>")
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		fatalf("read %s: %v", target, err)
	}
	var sub publish.Submission
	if err := json.Unmarshal(raw, &sub); err != nil {
		fatalf("parse submission %s: %v", target, err)
	}
	if errs := sub.Validate(); len(errs) != 0 {
		fmt.Fprintln(os.Stderr, "submission validation failed:")
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "  - %v\n", e)
		}
		os.Exit(1)
	}
	// A successful build self-verifies each platform through the catalogue gate
	// (SPEC §7.1); we then run the explicit per-bundle checks too.
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		fatalf("%v", err)
	}
	b, err := publish.BuildBundle(sub.ToConfig(), priv)
	if err != nil {
		fatalf("build %s from submission: %v", sub.ID, err)
	}
	tmp, err := os.MkdirTemp("", "verify-submission")
	if err != nil {
		fatalf("%v", err)
	}
	defer os.RemoveAll(tmp)
	hasArtifacts := len(sub.Artifacts) > 0
	allOK := true
	for _, p := range b.Platforms {
		path := filepath.Join(tmp, p.TarballName)
		if err := os.WriteFile(path, p.Tarball, 0o644); err != nil {
			fatalf("%v", err)
		}
		entry, err := catalogue.EntryForBundle(path)
		if err != nil {
			fatalf("read built bundle %s: %v", p.TarballName, err)
		}
		r := catalogue.VerifyEntry(entry, nil)
		fmt.Printf("\n%s [%s]:\n", r.ID, p.TarballName)
		for _, c := range r.Checks {
			mark := "✓"
			if !c.OK {
				mark, allOK = "✗", false
			}
			fmt.Printf("  %s %-34s %s\n", mark, c.Name, c.Msg)
		}
		// Close the native-delivery gap: a submission that declares `artifacts`
		// MUST yield a bundle that actually carries install.json AND a manifest
		// wired for staging (the fs.write $APP grant the asset-aware adapter
		// needs). Absent either, the published app would have no binary to run —
		// exactly the silent breakage that slipped through before this check.
		if hasArtifacts {
			if msg, ok := checkStaging(p.Tarball); ok {
				fmt.Printf("  ✓ %-34s %s\n", "native-delivery (install.json)", msg)
			} else {
				fmt.Printf("  ✗ %-34s %s\n", "native-delivery (install.json)", msg)
				allOK = false
			}
		}
	}
	if !allOK {
		fmt.Fprintln(os.Stderr, "\nVERIFY FAILED — fix the ✗ items before submitting.")
		os.Exit(1)
	}
	fmt.Printf("\nVERIFY OK — built + verified %d platform(s) from the submission spec.\n", len(b.Platforms))
}

// cmdVerifyUpdate enforces the update ownership + monotonicity gate for a
// submission against the LIVE catalogue: if the app id already exists, the new
// version must be strictly higher and (for the PR/pointer path, where the bundle
// is pre-signed) the bundle's publisher key must equal the owning publisher pin.
// A brand-new id passes (first publish). This is the CI gate that makes "an
// update needs the same publishing key" enforceable without any stored secret —
// the publisher proves ownership by signing with the key already pinned.
//
// It accepts a submission.json (pointer or rich) or a submissions/<id>/ dir.
func cmdVerifyUpdate(args []string) {
	fs := flag.NewFlagSet("verify-update", flag.ExitOnError)
	_ = fs.Parse(args)
	target := fs.Arg(0)
	if target == "" {
		fatalf("usage: pilot-app verify-update <submissions/<id>/submission.json | submissions/<id>/>")
	}
	subPath, subDir := target, filepath.Dir(target)
	if fi, err := os.Stat(target); err == nil && fi.IsDir() {
		subPath, subDir = filepath.Join(target, "submission.json"), target
	}

	raw, err := os.ReadFile(subPath)
	if err != nil {
		fatalf("read %s: %v", subPath, err)
	}
	// One decode covers both shapes: a pointer carries `bundle`; a rich submission
	// carries backend/methods. Either way we need id + version, and for a pointer
	// we additionally recover the signer pubkey from the built bundle's manifest.
	var ptr struct {
		ID      string `json:"id"`
		Version string `json:"version"`
		Bundle  string `json:"bundle"`
	}
	if err := json.Unmarshal(raw, &ptr); err != nil {
		fatalf("parse %s: %v", subPath, err)
	}
	if ptr.ID == "" || ptr.Version == "" {
		fatalf("submission %s is missing id/version", subPath)
	}

	newPublisher := ""
	if ptr.Bundle != "" {
		// Pointer path: the bundle is pre-built and publisher-signed — recover the
		// signer from its manifest so we can enforce the same-key rule.
		facts, err := catalogue.ReadBundleFacts(filepath.Join(subDir, ptr.Bundle))
		if err != nil {
			fatalf("read bundle facts for %s: %v", ptr.Bundle, err)
		}
		newPublisher = facts.Publisher
		if facts.Version != ptr.Version {
			fatalf("submission version %q != bundle manifest version %q", ptr.Version, facts.Version)
		}
	}

	owners, err := catalogue.FetchOwners(catalogue.CatalogueURL())
	if err != nil {
		fatalf("update gate: %v", err)
	}
	r := catalogue.CheckUpdate(owners, ptr.ID, ptr.Version, newPublisher)

	fmt.Printf("update gate for %s v%s:\n", ptr.ID, ptr.Version)
	allOK := true
	for _, c := range r.Checks {
		mark := "✓"
		if !c.OK {
			mark, allOK = "✗", false
		}
		fmt.Printf("  %s %-34s %s\n", mark, c.Name, c.Msg)
	}
	if !allOK {
		fmt.Fprintln(os.Stderr, "\nUPDATE GATE FAILED — an update must increase the version and be signed by the original publisher key.")
		os.Exit(1)
	}
	fmt.Println("\nUPDATE GATE OK.")
}

// checkStaging inspects one built platform tarball and confirms the native
// asset-delivery wiring is present: install.json (the registry staging spec the
// adapter reads at startup) AND a manifest whose grants include fs.write $APP
// (the capability the staging runtime needs to write the fetched binary). It
// returns a human-readable message and whether the bundle is staging-ready.
//
// This is what closes the false-pass: before this, a submission that declared
// artifacts but whose build dropped them (no install.json, no staging grant)
// still passed verify-submission, because the catalogue gate only checks the
// binary sha/signature and is blind to install.json.
func checkStaging(tarball []byte) (string, bool) {
	files, err := tarballFiles(tarball)
	if err != nil {
		return fmt.Sprintf("read bundle: %v", err), false
	}
	spec, hasInstall := files["./install.json"]
	if !hasInstall {
		if _, alt := files["install.json"]; alt {
			spec, hasInstall = files["install.json"], true
		}
	}
	if !hasInstall {
		return "submission declares artifacts but the built bundle has NO install.json — the adapter would have no binary to stage", false
	}
	// install.json must name a command and at least one asset.
	var is struct {
		Command string `json:"command"`
		Assets  []struct {
			ExecPath string `json:"exec_path"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(spec, &is); err != nil {
		return fmt.Sprintf("install.json present but unparseable: %v", err), false
	}
	if len(is.Assets) == 0 {
		return "install.json present but lists no assets", false
	}
	// The manifest must grant fs.write $APP so the staging runtime can write the
	// fetched binary under $APP — proof the adapter is wired for delivery, not
	// just that the spec file rode along.
	mfRaw, hasMf := files["./manifest.json"]
	if !hasMf {
		mfRaw, hasMf = files["manifest.json"]
	}
	if !hasMf {
		return "install.json present but manifest.json missing from bundle", false
	}
	var mf struct {
		Grants []struct {
			Cap    string `json:"cap"`
			Target string `json:"target"`
		} `json:"grants"`
	}
	if err := json.Unmarshal(mfRaw, &mf); err != nil {
		return fmt.Sprintf("manifest.json unparseable: %v", err), false
	}
	stagingGrant := false
	for _, g := range mf.Grants {
		if g.Cap == "fs.write" && g.Target == "$APP" {
			stagingGrant = true
			break
		}
	}
	if !stagingGrant {
		return "install.json present but manifest lacks the fs.write $APP grant the staging adapter needs", false
	}
	return fmt.Sprintf("install.json + staging grant present (%d asset(s))", len(is.Assets)), true
}

// tarballFiles reads a gzipped tar bundle into a map of header-name → contents.
func tarballFiles(tarball []byte) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	out := map[string][]byte{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out[hdr.Name] = b
	}
	return out, nil
}

func cmdSubmit(args []string) {
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	dir := fs.String("C", ".", "project dir (holds manifest.json + the built tarball)")
	prepare := fs.String("prepare", "", "write a submission payload into <dir>/submissions/<id>/ (a checkout/fork of pilot-protocol/app-template) to commit + PR")
	_ = fs.Parse(args)

	mfPath := filepath.Join(*dir, "manifest.json")
	mfRaw, err := os.ReadFile(mfPath)
	if err != nil {
		fatalf("read %s: %v (run `make package` first)", mfPath, err)
	}
	var m struct {
		ID         string `json:"id"`
		AppVersion string `json:"app_version"`
	}
	if err := json.Unmarshal(mfRaw, &m); err != nil {
		fatalf("parse manifest: %v", err)
	}
	ns := m.ID
	if i := strings.LastIndexByte(m.ID, '.'); i >= 0 {
		ns = m.ID[i+1:]
	}
	tarball := filepath.Join(*dir, fmt.Sprintf("%s-%s.tar.gz", m.ID, m.AppVersion))
	if _, err := os.Stat(tarball); err != nil {
		fatalf("bundle %s not found — run `make package` first", tarball)
	}

	// Pre-flight: run the same gate CI will run.
	fmt.Println("pre-flight (catalogue review gate):")
	entry, err := catalogue.EntryForBundle(tarball)
	if err != nil {
		fatalf("%v", err)
	}
	res := catalogue.VerifyEntry(entry, nil)
	for _, c := range res.Checks {
		mark := "✓"
		if !c.OK {
			mark = "✗"
		}
		fmt.Printf("  %s %s — %s\n", mark, c.Name, c.Msg)
	}
	if !res.OK() {
		fatalf("pre-flight failed; fix the ✗ items before submitting")
	}

	sum := sha256.Sum256(mustRead(tarball))
	tarSHA := hex.EncodeToString(sum[:])

	// Single-central-repo path: write a submission payload into a checkout/fork
	// of pilot-protocol/app-template, which the client commits + PRs. CI verifies
	// it; on merge, automation publishes to pilot-protocol/catalog + the catalogue.
	if *prepare != "" {
		subDir := filepath.Join(*prepare, "submissions", m.ID)
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			fatalf("mkdir %s: %v", subDir, err)
		}
		bundleName := fmt.Sprintf("%s-%s.tar.gz", m.ID, m.AppVersion)
		if err := os.WriteFile(filepath.Join(subDir, bundleName), mustRead(tarball), 0o644); err != nil {
			fatalf("copy bundle: %v", err)
		}
		// Enrich the project's metadata.json (catalogue v2 store-page record) with
		// the runtime facts only known post-build: publisher pubkey + sizes.
		writeEnrichedMetadata(*dir, subDir, tarball)
		meta := fmt.Sprintf(`{
  "id": %q,
  "version": %q,
  "namespace": %q,
  "description": %q,
  "bundle": %q,
  "bundle_sha256": %q
}
`, m.ID, m.AppVersion, ns, "<one accurate line — edit me>", bundleName, tarSHA)
		if err := os.WriteFile(filepath.Join(subDir, "submission.json"), []byte(meta), 0o644); err != nil {
			fatalf("write submission.json: %v", err)
		}
		fmt.Printf("\nsubmission payload written to %s/\n  %s\n  submission.json (edit the description)\n", subDir, bundleName)
		fmt.Printf("\nnext:\n  cd %s\n  git add submissions/%s && git commit -m %q\n  gh pr create   # against pilot-protocol/app-template\n",
			*prepare, m.ID, "submit "+m.ID+" v"+m.AppVersion)
		return
	}

	// Direct path (maintainers with org push): the raw release + catalogue steps.
	tag := fmt.Sprintf("%s-v%s", ns, m.AppVersion)
	bundleURL := fmt.Sprintf("https://github.com/pilot-protocol/catalog/releases/download/%s/%s-%s.tar.gz", tag, m.ID, m.AppVersion)
	relCmd := fmt.Sprintf("gh release create %s -R pilot-protocol/catalog %s -t %q -n %q",
		tag, tarball, m.ID+" v"+m.AppVersion, "Pilot app-store bundle for "+m.ID)
	catEntry := fmt.Sprintf(`{
  "id": %q,
  "version": %q,
  "description": "<one accurate line>",
  "bundle_url": %q,
  "bundle_sha256": %q
}`, m.ID, m.AppVersion, bundleURL, tarSHA)

	fmt.Printf("\nTo publish via the single central repo, run `pilot-app submit -C %s --prepare <app-template-fork>`.\n", *dir)
	fmt.Printf("\nDirect path (org maintainers only):\n── Step 1: release on pilot-protocol/catalog ──\n%s\n", relCmd)
	fmt.Printf("\n── Step 2: add to catalogue/catalogue.json on TeoSlayer/pilotprotocol@main (PR) ──\n%s\n", catEntry)
}

// writeEnrichedMetadata reads the project's metadata.json (from init), fills the
// post-build runtime facts (publisher pubkey + sizes), and writes it into the
// submission dir. If the project predates metadata.json, it warns and skips
// (the listing falls back to the basic catalogue fields).
func writeEnrichedMetadata(projectDir, subDir, tarball string) {
	raw, err := os.ReadFile(filepath.Join(projectDir, "metadata.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "  warning: no metadata.json in project (re-run `pilot-app init` to get a v2 store listing); skipping\n")
		return
	}
	var md scaffold.Metadata
	if err := json.Unmarshal(raw, &md); err != nil {
		fatalf("parse project metadata.json: %v", err)
	}
	facts, err := catalogue.ReadBundleFacts(tarball)
	if err != nil {
		fatalf("read bundle facts: %v", err)
	}
	md.Vendor.PublisherPubkey = facts.Publisher
	md.Size = scaffold.MetaSize{BundleBytes: facts.BundleBytes, InstalledBytes: facts.InstalledBytes}
	out, err := json.MarshalIndent(md, "", "  ")
	if err != nil {
		fatalf("marshal metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "metadata.json"), append(out, '\n'), 0o644); err != nil {
		fatalf("write submission metadata.json: %v", err)
	}
}

func mustRead(p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		fatalf("read %s: %v", p, err)
	}
	return b
}

func loadAndCheck(path string) *scaffold.Config {
	data, err := os.ReadFile(path)
	if err != nil {
		fatalf("read %s: %v", path, err)
	}
	cfg, err := scaffold.Parse(data)
	if err != nil {
		fatalf("%v", err)
	}
	cfg.Resolve()
	if errs := cfg.Validate(); len(errs) > 0 {
		fmt.Fprintf(os.Stderr, "spec has %d error(s):\n", len(errs))
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "  - %v\n", e)
		}
		os.Exit(1)
	}
	return cfg
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "pilot-app: "+format+"\n", a...)
	os.Exit(1)
}
