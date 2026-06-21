package publish

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"testing"
)

// magicForPlatform returns the executable magic an os should produce.
func isELF(b []byte) bool {
	return len(b) >= 4 && b[0] == 0x7f && b[1] == 'E' && b[2] == 'L' && b[3] == 'F'
}
func isMachO(b []byte) bool {
	if len(b) < 4 {
		return false
	}
	// little-endian Mach-O (0xfeedfacf / 0xfeedface) lands as cf/ce fa ed fe.
	return b[0] == 0xcf || b[0] == 0xce
}

// binFromTarball pulls bin/<binary> out of a gzipped bundle tarball.
func binFromTarball(t *testing.T, tarball []byte, binName string) []byte {
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
		if h.Name == "./bin/"+binName {
			b, _ := io.ReadAll(tr)
			return b
		}
	}
	t.Fatalf("bin/%s not found in tarball", binName)
	return nil
}

// TestBuildBundle_AllPlatforms is the regression guard for multi-platform
// publishing: one BuildBundle call must emit a real, correctly-formatted
// binary for every target in DefaultPlatforms, with linux/amd64 as the
// back-compat primary.
func TestBuildBundle_AllPlatforms(t *testing.T) {
	if testing.Short() {
		t.Skip("builds 4 binaries; skipped under -short")
	}
	priv, err := LoadOrCreateKey(t.TempDir() + "/k.key")
	if err != nil {
		t.Fatal(err)
	}
	cfg := sampleSubmission().ToConfig()
	b, err := BuildBundle(cfg, priv)
	if err != nil {
		t.Fatalf("BuildBundle: %v", err)
	}

	if len(b.Platforms) != len(DefaultPlatforms) {
		t.Fatalf("want %d platforms, got %d", len(DefaultPlatforms), len(b.Platforms))
	}
	if p := b.Primary(); p == nil || p.Platform != "linux/amd64" || b.SHA256 != p.SHA256 {
		t.Fatalf("primary should be linux/amd64 and mirror Bundle.SHA256; got %+v", p)
	}

	seenSHA := map[string]bool{}
	want := map[string]func([]byte) bool{
		"linux/amd64":  isELF,
		"linux/arm64":  isELF,
		"darwin/arm64": isMachO,
		"darwin/amd64": isMachO,
	}
	for _, p := range b.Platforms {
		check, ok := want[p.Platform]
		if !ok {
			t.Errorf("unexpected platform %s", p.Platform)
			continue
		}
		if seenSHA[p.SHA256] {
			t.Errorf("duplicate tarball sha across platforms at %s — same binary built twice?", p.Platform)
		}
		seenSHA[p.SHA256] = true
		bin := binFromTarball(t, p.Tarball, cfg.BinaryName)
		if !check(bin) {
			t.Errorf("%s: binary magic %x is wrong for the platform", p.Platform, bin[:4])
		}
		delete(want, p.Platform)
	}
	if len(want) != 0 {
		t.Errorf("missing platforms: %v", want)
	}
}
