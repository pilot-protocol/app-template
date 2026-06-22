package scaffold

import (
	"encoding/json"
	"path"
	"sort"
)

// InstallSpec is the staging contract shipped as install.json in the bundle. The
// generated adapter reads it at startup and, for the asset(s) matching the host
// os/arch, fetches → sha256-verifies → stages under $APP/<exec_path> → runs any
// install args, in ascending `order`. It is the machine-readable form of the
// publisher's Artifacts step (R2 location + install order + install params).
//
// Integrity: each asset carries its own sha256, and the whole bundle tarball is
// itself sha-pinned in the catalogue (bundle_sha256), so install.json cannot be
// altered without failing the install-time tarball check.
type InstallSpec struct {
	Schema  int          `json:"schema"`  // 1
	App     string       `json:"app"`     // io.pilot.<name>
	Version string       `json:"version"` // app_version
	Command string       `json:"command"` // base command the adapter execs (proc.exec target)
	Assets  []InstallAsset `json:"assets"`
}

// InstallAsset mirrors scaffold.Asset in the on-disk install spec.
type InstallAsset struct {
	Role     string   `json:"role"`      // binary | data
	OS       string   `json:"os"`        // linux | darwin
	Arch     string   `json:"arch"`      // amd64 | arm64
	URL      string   `json:"url"`       // https (R2 public URL)
	SHA256   string   `json:"sha256"`    // 64-hex of the downloaded object
	Unpack   string   `json:"unpack"`    // "" | "tar.gz"
	ExecPath string   `json:"exec_path"` // dest under $APP, or path inside the extracted tree
	Order    int      `json:"order"`     // ascending install sequence
	Args     []string `json:"args"`      // optional post-stage invocation
}

// marshalInstallSpec builds install.json from cfg.Assets, sorted by install
// order so the file is deterministic and the adapter can stage top-to-bottom.
func marshalInstallSpec(c *Config) ([]byte, error) {
	var cmd string
	if len(c.Backend.Command) > 0 {
		cmd = c.Backend.Command[0]
	}
	spec := InstallSpec{Schema: 1, App: c.ID, Version: c.AppVersion, Command: cmd}
	for _, a := range c.Assets {
		role := a.Role
		if role == "" {
			role = "binary"
		}
		spec.Assets = append(spec.Assets, InstallAsset{
			Role: role, OS: a.OS, Arch: a.Arch, URL: a.URL, SHA256: a.SHA256,
			Unpack: a.Unpack, ExecPath: path.Clean(a.ExecPath), Order: a.Order, Args: a.Args,
		})
	}
	sort.SliceStable(spec.Assets, func(i, j int) bool { return spec.Assets[i].Order < spec.Assets[j].Order })
	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
