package scaffold

import (
	"bytes"
	"embed"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

//go:embed templates/*.tmpl
var templates embed.FS

// file pairs an output path (relative to the project root) with the template
// that renders it.
type file struct {
	out  string
	tmpl string
}

// Generate renders a full adapter project for cfg into outDir. cfg must already
// be Resolve()d and Validate()d. Returns the list of written paths.
func Generate(cfg *Config, outDir string) ([]string, error) {
	files := []file{
		{filepath.Join("cmd", cfg.BinaryName, "main.go"), "main.go.tmpl"},
		{"manifest.json", "manifest.json.tmpl"},
		{"go.mod", "go.mod.tmpl"},
		{"Makefile", "Makefile.tmpl"},
		{"README.md", "README.md.tmpl"},
		{".gitignore", "gitignore.tmpl"},
	}
	switch cfg.Backend.Type {
	case "http":
		files = append(files, file{filepath.Join("internal", "backend", "client.go"), "client_http.go.tmpl"})
		if cfg.Backend.X402 != nil {
			files = append(files, file{filepath.Join("internal", "backend", "x402.go"), "x402.go.tmpl"})
		}
		if cfg.Managed() {
			files = append(files, file{filepath.Join("internal", "backend", "signer.go"), "signer.go.tmpl"})
		}
	case "cli":
		files = append(files, file{filepath.Join("internal", "backend", "exec.go"), "client_cli.go.tmpl"})
		// Native-binary delivery: emit the staging runtime only when the app
		// actually ships assets (an already-installed cli needs no stager).
		if cfg.HasAssets() {
			files = append(files, file{filepath.Join("internal", "backend", "stage.go"), "stage.go.tmpl"})
		}
	}

	var written []string

	// metadata.json (catalogue v2 store-page record) is built from a Go model,
	// not a text template — JSON is safer to assemble structurally.
	meta, err := marshalMetadata(BuildMetadata(cfg))
	if err != nil {
		return written, fmt.Errorf("build metadata.json: %w", err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return written, fmt.Errorf("mkdir %s: %w", outDir, err)
	}
	metaDest := filepath.Join(outDir, "metadata.json")
	if err := os.WriteFile(metaDest, meta, 0o644); err != nil {
		return written, fmt.Errorf("write metadata.json: %w", err)
	}
	written = append(written, "metadata.json")

	// install.json (the registry staging spec) ships in the bundle alongside the
	// manifest. The adapter reads it at startup to fetch/verify/stage each asset.
	// Built from a Go model (not a text template) so the JSON is assembled safely.
	if cfg.HasAssets() {
		spec, err := marshalInstallSpec(cfg)
		if err != nil {
			return written, fmt.Errorf("build install.json: %w", err)
		}
		if err := os.WriteFile(filepath.Join(outDir, "install.json"), spec, 0o644); err != nil {
			return written, fmt.Errorf("write install.json: %w", err)
		}
		written = append(written, "install.json")

		script, err := renderInstallScript(cfg)
		if err != nil {
			return written, fmt.Errorf("build install.sh: %w", err)
		}
		if err := os.WriteFile(filepath.Join(outDir, "install.sh"), script, 0o755); err != nil {
			return written, fmt.Errorf("write install.sh: %w", err)
		}
		written = append(written, "install.sh")
	}

	for _, f := range files {
		rendered, err := render(f.tmpl, cfg)
		if err != nil {
			return written, fmt.Errorf("render %s: %w", f.tmpl, err)
		}
		if strings.HasSuffix(f.out, ".go") {
			formatted, ferr := format.Source(rendered)
			if ferr != nil {
				return written, fmt.Errorf("format %s (generated invalid Go): %w", f.out, ferr)
			}
			rendered = formatted
		}
		dest := filepath.Join(outDir, f.out)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return written, fmt.Errorf("mkdir %s: %w", filepath.Dir(dest), err)
		}
		if err := os.WriteFile(dest, rendered, 0o644); err != nil {
			return written, fmt.Errorf("write %s: %w", dest, err)
		}
		written = append(written, f.out)
	}
	return written, nil
}

func render(name string, cfg *Config) ([]byte, error) {
	t, err := template.New(name).ParseFS(templates, "templates/"+name)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, cfg); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
