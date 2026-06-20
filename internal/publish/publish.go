package publish

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const publishRepo = "pilot-protocol/app-template"

// TriggerPublish stages a stored submission under submissions/<appID>/ on a new
// branch of pilot-protocol/app-template and opens a PR to main. Merging that PR
// fires the publish-on-merge workflow (release on catalog + catalogue v2 PR on
// the platform repo). This is the approval action: the GUI approves, a PR is
// opened, and merging it publishes — so it works under a protected main (a
// direct push to main is rejected by branch protection). Returns the PR URL.
// token needs contents:write + pull_requests:write on app-template.
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
	url := fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", token, publishRepo)
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

	// Unique branch per publish so re-approvals never collide with an open PR.
	branch := "publish/" + appID + "-" + strconv.FormatInt(time.Now().Unix(), 10)
	for _, a := range [][]string{
		{"config", "user.name", "Alex Godoroja"},
		{"config", "user.email", "alex@vulturelabs.io"},
		{"checkout", "-b", branch},
		{"add", "-A"},
		{"commit", "-m", "publish: " + appID + " (approved via submission server)"},
		{"push", "origin", branch}, // a branch, not main — branch protection doesn't block this
	} {
		if out, err := git(repo, a...); err != nil {
			return "", fmt.Errorf("git %s: %w\n%s", a[0], err, redact(out, token))
		}
	}
	return openPR(token, branch, appID)
}

// openPR opens a PR (branch -> main) on the publish repo via the GitHub API and
// returns its URL.
func openPR(token, branch, appID string) (string, error) {
	payload, _ := json.Marshal(map[string]string{
		"title": "publish: " + appID,
		"head":  branch,
		"base":  "main",
		"body": "Automated submission from the publish server (approved in the admin board).\n\n" +
			"Merging this adds `submissions/" + appID + "/` and fires the publish-on-merge workflow " +
			"(catalog release + catalogue PR).",
	})
	req, err := http.NewRequest("POST", "https://api.github.com/repos/"+publishRepo+"/pulls", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("open PR: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("open PR: github %d: %s", resp.StatusCode, redact(strings.TrimSpace(string(body)), token))
	}
	var pr struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &pr); err != nil || pr.HTMLURL == "" {
		return "", fmt.Errorf("open PR: could not parse PR URL")
	}
	return pr.HTMLURL, nil
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
