package publish

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pilot-protocol/app-store/pkg/manifest"
	"github.com/pilot-protocol/app-template/internal/catalogue"
	"github.com/pilot-protocol/app-template/internal/scaffold"
)

// DefaultPlatforms is the set of OS/arch targets every published app is built
// for. The adapter is pure Go (CGO_ENABLED=0), so all four cross-compile from
// any one build host — the reason a single-platform bundle was a build-host
// accident, not a requirement. linux/amd64 is first so it's the back-compat
// primary (the bare bundle_url older clients fetch).
var DefaultPlatforms = []string{"linux/amd64", "linux/arm64", "darwin/arm64", "darwin/amd64"}

// PlatformBundle is one OS/arch's signed tarball.
type PlatformBundle struct {
	Platform    string // "darwin/arm64"
	Tarball     []byte
	TarballName string // io.pilot.x-0.1.0-darwin-arm64.tar.gz
	SHA256      string
}

// Bundle is the output of a server-side build: the per-platform signed tarballs
// plus the derived metadata artifact, ready to store and (on approval) publish.
// Tarball/TarballName/SHA256 mirror the linux/amd64 primary so existing
// single-platform callers keep working unchanged.
type Bundle struct {
	Tarball      []byte
	TarballName  string // io.pilot.x-0.1.0-linux-amd64.tar.gz (primary)
	SHA256       string // primary tarball sha
	Namespace    string
	MetadataJSON []byte           // enriched catalogue-v2 metadata.json
	Platforms    []PlatformBundle // every target in DefaultPlatforms, primary included
}

// Primary returns the linux/amd64 bundle (the back-compat fallback), or the
// first platform built if linux/amd64 somehow isn't present.
func (b *Bundle) Primary() *PlatformBundle {
	for i := range b.Platforms {
		if b.Platforms[i].Platform == "linux/amd64" {
			return &b.Platforms[i]
		}
	}
	if len(b.Platforms) > 0 {
		return &b.Platforms[0]
	}
	return nil
}

// BuildBundle runs the full publisher pipeline ON THE SERVER for cfg: scaffold
// once → for each target { cross-compile → sha-pin → sign (platform key) → tar
// → self-verify against the catalogue gate }. All computation is server-side;
// the caller only supplies the validated spec.
func BuildBundle(cfg *scaffold.Config, priv ed25519.PrivateKey) (*Bundle, error) {
	tmp, err := os.MkdirTemp("", "pilot-build-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	if _, err := scaffold.Generate(cfg, tmp); err != nil {
		return nil, fmt.Errorf("scaffold: %w", err)
	}
	if out, err := runGo(tmp, nil, "mod", "tidy"); err != nil {
		return nil, fmt.Errorf("go mod tidy: %w\n%s", err, out)
	}
	mfRaw, err := os.ReadFile(filepath.Join(tmp, "manifest.json"))
	if err != nil {
		return nil, err
	}

	var (
		platforms       []PlatformBundle
		primaryBinBytes int
		primaryTarBytes int
	)
	for _, plat := range DefaultPlatforms {
		pb, binLen, err := buildPlatform(tmp, cfg, mfRaw, priv, plat)
		if err != nil {
			return nil, fmt.Errorf("build %s: %w", plat, err)
		}
		platforms = append(platforms, pb)
		if plat == "linux/amd64" {
			primaryBinBytes, primaryTarBytes = binLen, len(pb.Tarball)
		}
	}

	// Enrich the store-page metadata with post-build facts (primary's sizes).
	var md scaffold.Metadata
	if b, err := os.ReadFile(filepath.Join(tmp, "metadata.json")); err == nil {
		_ = json.Unmarshal(b, &md)
	}
	md.Vendor.PublisherPubkey = PublisherString(priv)
	md.Size = scaffold.MetaSize{BundleBytes: int64(primaryTarBytes), InstalledBytes: int64(primaryBinBytes)}
	metaJSON, err := json.MarshalIndent(md, "", "  ")
	if err != nil {
		return nil, err
	}

	b := &Bundle{Namespace: cfg.Namespace, MetadataJSON: append(metaJSON, '\n'), Platforms: platforms}
	if p := b.Primary(); p != nil {
		b.Tarball, b.TarballName, b.SHA256 = p.Tarball, p.TarballName, p.SHA256
	}
	return b, nil
}

// buildPlatform cross-compiles cfg's adapter for "<goos>/<goarch>" inside the
// already-scaffolded tmp dir, signs a single-binary manifest pinning that
// binary's sha, tars it, and self-verifies through the catalogue gate. Returns
// the bundle plus the uncompressed binary size (for metadata).
func buildPlatform(tmp string, cfg *scaffold.Config, mfRaw []byte, priv ed25519.PrivateKey, platform string) (PlatformBundle, int, error) {
	goos, goarch, ok := strings.Cut(platform, "/")
	if !ok {
		return PlatformBundle{}, 0, fmt.Errorf("bad platform %q (want os/arch)", platform)
	}
	binPath := filepath.Join(tmp, "bundle", "bin", cfg.BinaryName)
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		return PlatformBundle{}, 0, err
	}
	// CGO off so cross-compiles need no target toolchain/SDK (pure-Go adapter).
	env := []string{"CGO_ENABLED=0", "GOOS=" + goos, "GOARCH=" + goarch}
	if out, err := runGo(tmp, env, "build", "-trimpath", "-o", binPath, "./cmd/"+cfg.BinaryName); err != nil {
		return PlatformBundle{}, 0, fmt.Errorf("go build: %w\n%s", err, out)
	}

	binBytes, err := os.ReadFile(binPath)
	if err != nil {
		return PlatformBundle{}, 0, err
	}
	binSum := sha256.Sum256(binBytes)
	m, err := manifest.Parse(mfRaw)
	if err != nil {
		return PlatformBundle{}, 0, fmt.Errorf("parse generated manifest: %w", err)
	}
	m.Binary.SHA256 = hex.EncodeToString(binSum[:])
	if err := SignManifest(m, priv); err != nil {
		return PlatformBundle{}, 0, err
	}
	signedMf, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return PlatformBundle{}, 0, err
	}
	if err := os.WriteFile(filepath.Join(tmp, "bundle", "manifest.json"), signedMf, 0o644); err != nil {
		return PlatformBundle{}, 0, err
	}

	tarball, err := tarGz(filepath.Join(tmp, "bundle"))
	if err != nil {
		return PlatformBundle{}, 0, fmt.Errorf("tar: %w", err)
	}
	sum := sha256.Sum256(tarball)
	tarName := fmt.Sprintf("%s-%s-%s-%s.tar.gz", cfg.ID, cfg.AppVersion, goos, goarch)

	// Self-verify through the exact catalogue gate CI/publish use.
	tf := filepath.Join(tmp, tarName)
	if err := os.WriteFile(tf, tarball, 0o644); err != nil {
		return PlatformBundle{}, 0, err
	}
	entry, err := catalogue.EntryForBundle(tf)
	if err != nil {
		return PlatformBundle{}, 0, err
	}
	if res := catalogue.VerifyEntry(entry, nil); !res.OK() {
		return PlatformBundle{}, 0, fmt.Errorf("built bundle failed self-verification: %s", firstFail(res))
	}
	return PlatformBundle{
		Platform: platform, Tarball: tarball, TarballName: tarName, SHA256: hex.EncodeToString(sum[:]),
	}, len(binBytes), nil
}

func runGo(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func firstFail(r catalogue.Result) string {
	for _, c := range r.Checks {
		if !c.OK {
			return c.Name + ": " + c.Msg
		}
	}
	return "unknown"
}

// tarGz writes dir's contents (recursively) as a gzipped tar, paths relative to
// dir — the same layout `tar -C bundle -czf` produces.
func tarGz(dir string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = "./" + filepath.ToSlash(rel)
		if fi.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = tw.Write(b)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
