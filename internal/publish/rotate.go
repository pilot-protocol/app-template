package publish

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const catalogueRepo = "pilot-protocol/pilotprotocol"

// pubKeyRe matches a publisher pin: ed25519:<std-base64 of a 32-byte key>.
var pubKeyRe = regexp.MustCompile(`^ed25519:[A-Za-z0-9+/]{43}=$`)

// ValidPublisherPin reports whether s is a well-formed ed25519 publisher pin.
func ValidPublisherPin(s string) bool {
	if !pubKeyRe.MatchString(s) {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, "ed25519:"))
	return err == nil && len(raw) == ed25519.PublicKeySize
}

// RotatePublisher re-points the catalogue `publisher` pin for appID to
// newPublisher and opens a PR to the platform repo with the catalogue re-signed,
// so a maintainer (the admin) can hand an app to a new owning key — e.g. after a
// key loss or a publisher handoff. Future updates must then be signed by the new
// key (the same gate, now against the new pin).
//
// catalogToken needs contents:write + pull_requests:write on the platform repo;
// signKeyHex is the hex ed25519 catalogue signing key (CATALOG_SIGN_KEY) — it must
// match the embedded catalogue trust pubkey, or we refuse to write a dead
// signature. Returns the PR URL.
//
// NOTE (documented for the operator): the runtime trust anchor checks the
// installed bundle's signer against this pin, so after a rotation the new owner
// must publish an update signed with the new key before existing installs will
// re-validate. Rotation changes WHO may publish; it does not re-sign the live bundle.
func RotatePublisher(appID, newPublisher, catalogToken, signKeyHex string) (string, error) {
	if !subID.MatchString(appID) {
		return "", fmt.Errorf("invalid app id %q", appID)
	}
	if !ValidPublisherPin(newPublisher) {
		return "", fmt.Errorf("new_publisher must be ed25519:<base64 32-byte key>")
	}
	if catalogToken == "" {
		return "", fmt.Errorf("no catalogue token configured (set CATALOG_PUBLISH_TOKEN)")
	}
	signKey, err := decodeSignKey(signKeyHex)
	if err != nil {
		return "", err
	}

	tmp, err := os.MkdirTemp("", "pilot-rotate-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	repo := filepath.Join(tmp, "pilotprotocol")
	url := fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", catalogToken, catalogueRepo)
	if out, err := git(tmp, "clone", "--depth", "1", url, repo); err != nil {
		return "", fmt.Errorf("clone: %w\n%s", err, redact(out, catalogToken))
	}

	catPath := filepath.Join(repo, "catalogue", "catalogue.json")
	raw, err := os.ReadFile(catPath)
	if err != nil {
		return "", fmt.Errorf("read catalogue: %w", err)
	}
	updated, oldPub, err := setPublisher(raw, appID, newPublisher)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(catPath, updated, 0o644); err != nil {
		return "", err
	}
	// Re-sign the exact bytes we wrote and refresh the detached signature.
	sig := ed25519.Sign(signKey, updated)
	if err := os.WriteFile(catPath+".sig", []byte(base64.StdEncoding.EncodeToString(sig)+"\n"), 0o644); err != nil {
		return "", err
	}

	branch := "rotate/" + appID + "-" + strconv.FormatInt(time.Now().Unix(), 10)
	for _, a := range [][]string{
		{"config", "user.name", "Pilot Publish Server"},
		{"config", "user.email", "ops@pilotprotocol.network"},
		{"checkout", "-b", branch},
		{"add", "catalogue/catalogue.json", "catalogue/catalogue.json.sig"},
		{"commit", "-m", "rotate publisher key: " + appID},
		{"push", "origin", branch},
	} {
		if out, err := git(repo, a...); err != nil {
			return "", fmt.Errorf("git %s: %w\n%s", a[0], err, redact(out, catalogToken))
		}
	}
	body := fmt.Sprintf("Admin-initiated publisher key rotation for `%s`.\n\n"+
		"- old publisher: `%s`\n- new publisher: `%s`\n\n"+
		"Catalogue re-signed. After merge, the new owner must publish an update "+
		"signed with the new key for existing installs to re-validate against the new pin.",
		appID, oldPub, newPublisher)
	return openPRTo(catalogToken, catalogueRepo, branch, "rotate publisher key: "+appID, body)
}

// setPublisher returns the catalogue bytes with appID's `publisher` set to
// newPublisher, preserving every other field and app order. It re-marshals via a
// generic map (sorted keys) — the catalogue is consumed by JSON parsers, and the
// signature is over whatever bytes we emit, so key ordering is irrelevant.
func setPublisher(raw []byte, appID, newPublisher string) (out []byte, oldPub string, err error) {
	var cat map[string]any
	if err := json.Unmarshal(raw, &cat); err != nil {
		return nil, "", fmt.Errorf("parse catalogue: %w", err)
	}
	apps, ok := cat["apps"].([]any)
	if !ok {
		return nil, "", fmt.Errorf("catalogue has no apps array")
	}
	found := false
	for _, a := range apps {
		m, ok := a.(map[string]any)
		if !ok {
			continue
		}
		if m["id"] == appID {
			if p, _ := m["publisher"].(string); p != "" {
				oldPub = p
			}
			m["publisher"] = newPublisher
			found = true
			break
		}
	}
	if !found {
		return nil, "", fmt.Errorf("app %q is not in the catalogue", appID)
	}
	b, err := json.MarshalIndent(cat, "", "  ")
	if err != nil {
		return nil, "", err
	}
	return append(b, '\n'), oldPub, nil
}

// decodeSignKey parses the hex ed25519 catalogue signing key and refuses it
// unless its public half matches the embedded catalogue trust pubkey — so we
// never produce a signature pilotctl will reject at load.
func decodeSignKey(hexKey string) (ed25519.PrivateKey, error) {
	if strings.TrimSpace(hexKey) == "" {
		return nil, fmt.Errorf("no catalogue signing key configured (set CATALOG_SIGN_KEY)")
	}
	b, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil || len(b) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("CATALOG_SIGN_KEY must be a hex-encoded 64-byte ed25519 private key")
	}
	key := ed25519.PrivateKey(b)
	gotPub := base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	if want := catalogueTrustPubKeyB64(); want != "" && gotPub != want {
		return nil, fmt.Errorf("CATALOG_SIGN_KEY public half %s does not match the catalogue trust key", gotPub)
	}
	return key, nil
}

// catalogueTrustPubKeyB64 returns the std-base64 catalogue trust pubkey the
// signature must verify against (env override PILOT_CATALOGUE_PUBKEY wins; "-"
// disables the match for local testing).
func catalogueTrustPubKeyB64() string {
	if v := strings.TrimSpace(os.Getenv("PILOT_CATALOGUE_PUBKEY")); v != "" {
		if v == "-" {
			return ""
		}
		return v
	}
	return "iHdBWayA/hYjkwUOZopTXY70qOlR90d6ii/hin0ZMdI="
}

// openPRTo opens a PR (branch -> main) on the given repo via the GitHub API.
func openPRTo(token, repo, branch, title, body string) (string, error) {
	payload, _ := json.Marshal(map[string]string{
		"title": title, "head": branch, "base": "main", "body": body,
	})
	req, err := http.NewRequest("POST", "https://api.github.com/repos/"+repo+"/pulls", bytes.NewReader(payload))
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
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("open PR: github %d: %s", resp.StatusCode, redact(strings.TrimSpace(string(respBody)), token))
	}
	var pr struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(respBody, &pr); err != nil || pr.HTMLURL == "" {
		return "", fmt.Errorf("open PR: could not parse PR URL")
	}
	return pr.HTMLURL, nil
}
