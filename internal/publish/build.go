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

	"github.com/pilot-protocol/app-store/pkg/manifest"
	"github.com/pilot-protocol/app-template/internal/catalogue"
	"github.com/pilot-protocol/app-template/internal/scaffold"
)

// Bundle is the output of a server-side build: the signed tarball plus the
// derived submission/metadata artifacts, ready to store and (on approval) publish.
type Bundle struct {
	Tarball      []byte
	TarballName  string // io.pilot.x-0.1.0.tar.gz
	SHA256       string // tarball sha
	Namespace    string
	MetadataJSON []byte // enriched catalogue-v2 metadata.json
}

// BuildBundle runs the full publisher pipeline ON THE SERVER for cfg: scaffold →
// go build → sha-pin → sign (platform key) → tar → metadata → self-verify
// against the catalogue gate. All computation is server-side; the caller only
// supplies the validated spec.
func BuildBundle(cfg *scaffold.Config, priv ed25519.PrivateKey) (*Bundle, error) {
	tmp, err := os.MkdirTemp("", "pilot-build-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	if _, err := scaffold.Generate(cfg, tmp); err != nil {
		return nil, fmt.Errorf("scaffold: %w", err)
	}
	if out, err := runGo(tmp, "mod", "tidy"); err != nil {
		return nil, fmt.Errorf("go mod tidy: %w\n%s", err, out)
	}

	binPath := filepath.Join(tmp, "bundle", "bin", cfg.BinaryName)
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		return nil, err
	}
	if out, err := runGo(tmp, "build", "-o", binPath, "./cmd/"+cfg.BinaryName); err != nil {
		return nil, fmt.Errorf("go build: %w\n%s", err, out)
	}

	// sha-pin the built binary, then sign (the sha is in the signed payload).
	binBytes, err := os.ReadFile(binPath)
	if err != nil {
		return nil, err
	}
	binSum := sha256.Sum256(binBytes)
	mfRaw, err := os.ReadFile(filepath.Join(tmp, "manifest.json"))
	if err != nil {
		return nil, err
	}
	m, err := manifest.Parse(mfRaw)
	if err != nil {
		return nil, fmt.Errorf("parse generated manifest: %w", err)
	}
	m.Binary.SHA256 = hex.EncodeToString(binSum[:])
	if err := SignManifest(m, priv); err != nil {
		return nil, err
	}
	signedMf, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(tmp, "bundle", "manifest.json"), signedMf, 0o644); err != nil {
		return nil, err
	}

	tarball, err := tarGz(filepath.Join(tmp, "bundle"))
	if err != nil {
		return nil, fmt.Errorf("tar: %w", err)
	}
	sum := sha256.Sum256(tarball)
	tarName := fmt.Sprintf("%s-%s.tar.gz", cfg.ID, cfg.AppVersion)

	// Enrich the store-page metadata with post-build facts.
	var md scaffold.Metadata
	if b, err := os.ReadFile(filepath.Join(tmp, "metadata.json")); err == nil {
		_ = json.Unmarshal(b, &md)
	}
	md.Vendor.PublisherPubkey = PublisherString(priv)
	md.Size = scaffold.MetaSize{BundleBytes: int64(len(tarball)), InstalledBytes: int64(len(binBytes))}
	metaJSON, err := json.MarshalIndent(md, "", "  ")
	if err != nil {
		return nil, err
	}

	// Self-verify through the exact catalogue gate CI/publish use.
	tf := filepath.Join(tmp, tarName)
	if err := os.WriteFile(tf, tarball, 0o644); err != nil {
		return nil, err
	}
	entry, err := catalogue.EntryForBundle(tf)
	if err != nil {
		return nil, err
	}
	if res := catalogue.VerifyEntry(entry, nil); !res.OK() {
		return nil, fmt.Errorf("built bundle failed self-verification: %s", firstFail(res))
	}

	return &Bundle{
		Tarball: tarball, TarballName: tarName,
		SHA256: hex.EncodeToString(sum[:]), Namespace: cfg.Namespace,
		MetadataJSON: append(metaJSON, '\n'),
	}, nil
}

func runGo(dir string, args ...string) (string, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
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
