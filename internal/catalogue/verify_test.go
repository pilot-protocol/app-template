package catalogue

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"
)

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0},
		{"0.2.0", "0.1.9", 1},
		{"1.0.0", "0.9.9", 1},
		{"0.1.0", "0.1.1", -1},
		{"0.1.2", "0.1.10", -1}, // numeric, not lexical
	}
	for _, c := range cases {
		if got := compareSemver(c.a, c.b); got != c.want {
			t.Errorf("compareSemver(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestHasHelp(t *testing.T) {
	if !hasHelp([]string{"x.foo", "x.help"}) {
		t.Error("expected help detected")
	}
	if hasHelp([]string{"x.foo", "x.bar"}) {
		t.Error("expected no help")
	}
}

// TestExtractBundleLocatesBinaryByManifestPath builds a minimal tarball and
// confirms extractBundle reads manifest.json and follows binary.path.
func TestExtractBundleLocatesBinaryByManifestPath(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	write := func(name string, body []byte) {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write(body)
	}
	write("./manifest.json", []byte(`{"binary":{"path":"bin/app"}}`))
	write("./bin/app", []byte("ELF-ish bytes"))
	_ = tw.Close()
	_ = gz.Close()

	mf, bin, err := extractBundle(buf.Bytes())
	if err != nil {
		t.Fatalf("extractBundle: %v", err)
	}
	if !bytes.Contains(mf, []byte("binary")) {
		t.Errorf("manifest not returned: %s", mf)
	}
	if string(bin) != "ELF-ish bytes" {
		t.Errorf("binary mismatch: %q", bin)
	}
}

func TestExtractBundleRejectsMissingBinary(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte(`{"binary":{"path":"bin/missing"}}`)
	_ = tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = gz.Close()

	if _, _, err := extractBundle(buf.Bytes()); err == nil {
		t.Error("expected error for missing binary")
	}
}
