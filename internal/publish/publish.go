package publish

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// TriggerPublish pushes a stored submission into pilot-protocol/app-template's
// main under submissions/<appID>/, which fires the publish-on-merge workflow
// (release on catalog + catalogue v2 PR on the platform repo). This is the
// approval action: the GUI approves, the *workflow* publishes. Returns the
// commit sha. token needs push to app-template (admin bypass for protected main).
func TriggerPublish(subDir, appID, token string) (string, error) {
	if token == "" {
		return "", fmt.Errorf("no publish token configured (set PILOT_PUBLISH_TOKEN)")
	}
	tmp, err := os.MkdirTemp("", "pilot-pub-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	repo := tmp + "/app-template"
	url := fmt.Sprintf("https://x-access-token:%s@github.com/pilot-protocol/app-template.git", token)
	if out, err := git(tmp, "clone", "--depth", "1", url, repo); err != nil {
		return "", fmt.Errorf("clone: %w\n%s", err, redact(out, token))
	}

	dst := filepath.Join(repo, "submissions", appID)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return "", err
	}
	if err := copyDir(subDir, dst); err != nil {
		return "", fmt.Errorf("stage submission: %w", err)
	}

	for _, a := range [][]string{
		{"config", "user.name", "Alex Godoroja"},
		{"config", "user.email", "alex@vulturelabs.io"},
		{"add", "-A"},
		{"commit", "-m", "publish: " + appID + " (approved via submission server)"},
		{"push", "origin", "HEAD:main"},
	} {
		if out, err := git(repo, a...); err != nil {
			return "", fmt.Errorf("git %s: %w\n%s", a[0], err, redact(out, token))
		}
	}
	sha, _ := git(repo, "rev-parse", "--short", "HEAD")
	return trimNL(sha), nil
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// copyDir copies regular files from src into dst (one level — submission dirs
// are flat: bundle.tar.gz + submission.json + metadata.json).
func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == "status.json" { // status is server-internal
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func redact(s, token string) string {
	if token == "" {
		return s
	}
	return replaceAll(s, token, "***")
}

func replaceAll(s, old, new string) string {
	out := ""
	for {
		i := indexOf(s, old)
		if i < 0 {
			return out + s
		}
		out += s[:i] + new
		s = s[i+len(old):]
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
