package broker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// This file adds the PROVISIONING data plane on top of the managed-key broker.
// A provisioned app (Provision != nil) differs from a classic managed app: the
// broker mints a per-user derived key, records identity + source IP, keeps a
// credit ledger, and forwards cloud methods authenticated as that user through a
// CloudProvider. The master key stays in the broker; users only ever see their
// own derived key and their own owner-tagged resources.

// CloudProvider is the pluggable cloud data plane. Two impls ship:
//
//   - masterKeyProvider (default, "master"): one machine-scoped master key;
//     isolation is enforced BROKER-side by stamping every resource with the
//     owner and filtering reads to the caller. Works with today's smol key.
//   - tokenMintProvider ("tokenmint", dormant): mints a real per-user cloud token
//     from an admin key so the CLOUD enforces isolation. Built + unit-tested but
//     inactive until an admin key is configured.
type CloudProvider interface {
	// Push publishes a packed VM artifact for owner and creates an owner-tagged
	// machine. It returns the cloud's response to relay to the caller.
	Push(ctx context.Context, owner string, spec PushSpec, artifact []byte) (CloudResp, error)
	// List returns ONLY the machines owned by owner (isolation).
	List(ctx context.Context, owner string) (CloudResp, error)
	// Name identifies the provider (logging / diagnostics).
	Name() string
}

// PushSpec carries the non-artifact push parameters.
type PushSpec struct {
	Name  string // machine/artifact name (broker prefixes with the owner)
	Image string // optional: push from an existing OCI image ref instead of an artifact
	Arch  string // optional target arch
}

// CloudResp is a raw cloud HTTP result the broker relays to the caller verbatim.
type CloudResp struct {
	Status int
	Body   []byte
}

var errProviderDormant = errors.New("broker: tokenmint provider is dormant (no admin key wired)")

// ---- masterKeyProvider: broker-enforced isolation with a machine-scoped key ----

type masterKeyProvider struct {
	upstream    string // cloud base, e.g. https://api.smolmachines.com
	master      string // the cloud master key (smk_…); never leaves the broker
	ownerEnvKey string // machine env key stamped with the owner (e.g. PILOT_OWNER)
	uploadPath  string // artifact-upload endpoint (default /v1/artifacts)
	client      *http.Client
}

func newMasterKeyProvider(upstream, master, ownerEnvKey string) *masterKeyProvider {
	return &masterKeyProvider{
		upstream:    strings.TrimRight(upstream, "/"),
		master:      master,
		ownerEnvKey: ownerEnvKey,
		uploadPath:  "/v1/artifacts",
		client:      &http.Client{Timeout: 300 * time.Second},
	}
}

func (p *masterKeyProvider) Name() string { return "master" }

func (p *masterKeyProvider) do(ctx context.Context, method, path string, body []byte) (CloudResp, error) {
	req, err := http.NewRequestWithContext(ctx, method, p.upstream+path, bytes.NewReader(body))
	if err != nil {
		return CloudResp{}, err
	}
	req.Header.Set("Authorization", "Bearer "+p.master) // master key, broker-only
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return CloudResp{}, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return CloudResp{Status: resp.StatusCode, Body: rb}, nil
}

// ownerName namespaces a machine name by owner so listings don't collide between
// users and are visibly attributed. The cloud restricts machine names to
// [A-Za-z0-9_-], but a caller id is base64 (may contain '+' '/' '='), so the name
// portions are sanitized. The ISOLATION tag (env.PILOT_OWNER) keeps the exact,
// unsanitized caller id — only this cosmetic name is constrained.
func ownerName(owner, name string) string {
	short := sanitizeMachineName(owner)
	if len(short) > 8 {
		short = short[:8]
	}
	name = sanitizeMachineName(name)
	if name == "" {
		name = "vm"
	}
	return "smol-" + short + "-" + name
}

// sanitizeMachineName maps a string to the cloud's allowed machine-name charset
// (letters, digits, '-', '_'); every other rune becomes '-'.
func sanitizeMachineName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

func (p *masterKeyProvider) Push(ctx context.Context, owner string, spec PushSpec, artifact []byte) (CloudResp, error) {
	reference := spec.Image
	sourceType := "image"
	if len(artifact) > 0 {
		// Publish the packed VM, then reference the resulting artifact. The upload
		// endpoint is the pluggable seam over the cloud's artifact store (refcloud
		// implements it; the live-cloud OCI/group wiring is finalized with smolvm).
		up, err := p.uploadArtifact(ctx, owner, spec.Name, artifact)
		if err != nil {
			return CloudResp{}, err
		}
		if up.Status/100 != 2 {
			return up, nil // relay the cloud's error (e.g. quota) as-is
		}
		var parsed struct {
			Reference string `json:"reference"`
		}
		_ = json.Unmarshal(up.Body, &parsed)
		if parsed.Reference == "" {
			return CloudResp{}, fmt.Errorf("artifact upload returned no reference")
		}
		reference, sourceType = parsed.Reference, "smolmachine"
	}
	if reference == "" {
		return CloudResp{}, fmt.Errorf("push needs either an artifact or an image ref")
	}
	source := map[string]any{"type": sourceType, "reference": reference}
	if spec.Arch != "" {
		source["arch"] = spec.Arch
	}
	body, _ := json.Marshal(map[string]any{
		"source": source,
		"name":   ownerName(owner, spec.Name),
		"env":    map[string]string{p.ownerEnvKey: owner}, // the isolation tag
	})
	return p.do(ctx, http.MethodPost, "/v1/machines", body)
}

func (p *masterKeyProvider) uploadArtifact(ctx context.Context, owner, name string, artifact []byte) (CloudResp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.upstream+p.uploadPath, bytes.NewReader(artifact))
	if err != nil {
		return CloudResp{}, err
	}
	req.Header.Set("Authorization", "Bearer "+p.master)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Smol-Owner", owner)
	req.Header.Set("X-Smol-Name", ownerName(owner, name))
	resp, err := p.client.Do(req)
	if err != nil {
		return CloudResp{}, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return CloudResp{Status: resp.StatusCode, Body: rb}, nil
}

func (p *masterKeyProvider) List(ctx context.Context, owner string) (CloudResp, error) {
	resp, err := p.do(ctx, http.MethodGet, "/v1/machines", nil)
	if err != nil {
		return CloudResp{}, err
	}
	if resp.Status/100 != 2 {
		return resp, nil
	}
	// The cloud has no server-side filter, so the broker filters to the owner —
	// this is the isolation boundary for reads.
	var all []map[string]any
	if err := json.Unmarshal(resp.Body, &all); err != nil {
		return resp, nil // relay as-is if the shape is unexpected
	}
	mine := make([]map[string]any, 0, len(all))
	for _, m := range all {
		if env, ok := m["env"].(map[string]any); ok {
			if env[p.ownerEnvKey] == owner {
				mine = append(mine, m)
			}
		}
	}
	out, _ := json.Marshal(mine)
	return CloudResp{Status: 200, Body: out}, nil
}

// ---- tokenMintProvider: cloud-enforced isolation (dormant until an admin key) --

type tokenMintProvider struct {
	upstream string
	adminKey string
	client   *http.Client
}

func newTokenMintProvider(upstream, adminKey string) *tokenMintProvider {
	return &tokenMintProvider{
		upstream: strings.TrimRight(upstream, "/"),
		adminKey: adminKey,
		client:   &http.Client{Timeout: 300 * time.Second},
	}
}

func (p *tokenMintProvider) Name() string { return "tokenmint" }

// mintUserToken exchanges the admin key for a real per-user cloud token scoped to
// the owner (POST /v1/tokens). Wired for when an admin key exists; unused today.
func (p *tokenMintProvider) mintUserToken(ctx context.Context, owner string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"subject": owner,
		"scopes":  []string{"machine:create", "machine:read", "machine:exec", "machine:delete", "machine:files"},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.upstream+"/v1/tokens", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+p.adminKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("mint token: %s: %s", resp.Status, strings.TrimSpace(string(rb)))
	}
	var parsed struct {
		Token string `json:"token"`
		Key   string `json:"key"`
	}
	_ = json.Unmarshal(rb, &parsed)
	if parsed.Token != "" {
		return parsed.Token, nil
	}
	if parsed.Key != "" {
		return parsed.Key, nil
	}
	return "", fmt.Errorf("mint token: no token in response")
}

// Push/List are dormant: selecting this provider is gated on an admin key, and
// the live-cloud call shapes are finalized when that key exists. The mint path is
// implemented and unit-tested so activation is a config flip, not a rewrite.
func (p *tokenMintProvider) Push(ctx context.Context, owner string, spec PushSpec, artifact []byte) (CloudResp, error) {
	return CloudResp{}, errProviderDormant
}
func (p *tokenMintProvider) List(ctx context.Context, owner string) (CloudResp, error) {
	return CloudResp{}, errProviderDormant
}

// ---- IP trust ----

// IPTrust controls how the broker derives a caller's source IP. Behind nginx the
// broker trusts ONLY the header nginx sets from $remote_addr (default
// X-Real-IP). A client-supplied X-Forwarded-For is NEVER read, so a caller can't
// spoof its IP to dodge the per-IP identity cap.
type IPTrust struct {
	Header string // default "X-Real-IP"; empty ⇒ use the transport RemoteAddr only
}

func clientIP(h func(string) string, remoteAddr string, t IPTrust) string {
	if t.Header != "" {
		if v := strings.TrimSpace(h(t.Header)); v != "" {
			// take the first hop if the trusted proxy ever lists several
			if i := strings.IndexByte(v, ','); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
			return v
		}
	}
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// ---- reserved-route handlers ----

func (b *Broker) provStore() (ProvisionStore, bool) {
	ps, ok := b.Store.(ProvisionStore)
	return ps, ok
}

func (b *Broker) now() time.Time {
	if b.Verify.Now != nil {
		return b.Verify.Now()
	}
	return time.Now()
}

// pushEnvelope is the JSON body the adapter sends for a cloud push. The artifact
// is base64 so the whole envelope is covered by the request signature.
type pushEnvelope struct {
	Name     string `json:"name"`
	Image    string `json:"image"`
	Arch     string `json:"arch"`
	Artifact string `json:"artifact"` // base64 of the packed VM (optional if Image is set)
}

// serveProvisioned routes a verified request for a provisioned app: reserved
// identity/credit routes, plus cloud methods that debit credit and forward
// through the CloudProvider. The master key never reaches the caller.
func (b *Broker) serveProvisioned(w http.ResponseWriter, r *http.Request, app *AppEntry, mpath string, caller CallerID, body []byte) {
	p := app.Provision
	ip := clientIP(r.Header.Get, r.RemoteAddr, b.IPTrust)

	switch mpath {
	case p.ProvisionPath:
		b.serveProvision(w, app, caller, ip)
		return
	case p.BalancePath:
		b.serveBalance(w, app, caller)
		return
	case p.PushPath:
		b.servePush(w, r, app, caller, body)
		return
	case p.ListPath:
		b.serveCloudList(w, r, app, caller)
		return
	default:
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "method not allowed for this app: " + mpath})
		return
	}
}

// servePush debits the caller's credit, then forwards the artifact to the cloud
// as the owner. On any non-success it refunds so a failed push never burns credit.
func (b *Broker) servePush(w http.ResponseWriter, r *http.Request, app *AppEntry, caller CallerID, body []byte) {
	ps, ok := b.provStore()
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "store does not support provisioning"})
		return
	}
	if !app.breaker.Allow() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "upstream circuit open"})
		return
	}
	var env pushEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "push body must be JSON {name,image,arch,artifact}"})
		return
	}
	var artifact []byte
	if env.Artifact != "" {
		a, err := base64.StdEncoding.DecodeString(env.Artifact)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "artifact must be base64"})
			return
		}
		artifact = a
	}
	if len(artifact) == 0 && env.Image == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "push needs an artifact or an image ref"})
		return
	}

	cost := creditCost(app, app.Provision.PushPath)
	admitted, _, err := ps.Debit(app.ID, string(caller), cost)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "debit: " + err.Error()})
		return
	}
	if !admitted {
		writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "insufficient credit for push"})
		return
	}

	ctx := r.Context()
	if app.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(app.TimeoutMs)*time.Millisecond)
		defer cancel()
	}
	resp, err := app.provider.Push(ctx, string(caller), PushSpec{Name: env.Name, Image: env.Image, Arch: env.Arch}, artifact)
	if err != nil {
		app.breaker.Record(false)
		ps.Refund(app.ID, string(caller), cost)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "cloud push: " + err.Error()})
		return
	}
	// 5xx is an upstream fault (breaker + refund); any other non-2xx means the
	// push didn't take effect, so refund but don't trip the breaker.
	app.breaker.Record(resp.Status < 500)
	if resp.Status/100 != 2 {
		ps.Refund(app.ID, string(caller), cost)
	}
	relayCloud(w, resp)
}

// serveCloudList returns only the caller's own machines (free read, no debit).
func (b *Broker) serveCloudList(w http.ResponseWriter, r *http.Request, app *AppEntry, caller CallerID) {
	ctx := r.Context()
	if app.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(app.TimeoutMs)*time.Millisecond)
		defer cancel()
	}
	resp, err := app.provider.List(ctx, string(caller))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "cloud list: " + err.Error()})
		return
	}
	relayCloud(w, resp)
}

func relayCloud(w http.ResponseWriter, resp CloudResp) {
	w.Header().Set("Content-Type", "application/json")
	status := resp.Status
	if status == 0 {
		status = http.StatusBadGateway
	}
	w.WriteHeader(status)
	_, _ = w.Write(resp.Body)
}

// serveProvision handles POST /<app>/_provision: record identity + IP (spam
// controls), seed the credit ledger on first sight, and return the caller's
// derived key + balance. Idempotent — repeat calls return the same key.
func (b *Broker) serveProvision(w http.ResponseWriter, app *AppEntry, caller CallerID, ip string) {
	ps, ok := b.provStore()
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "store does not support provisioning"})
		return
	}
	p := app.Provision
	rec, err := ps.Provision(app.ID, string(caller), ip,
		p.SeedCredits, p.MaxIdentitiesPerIP, time.Duration(p.MintCooldownMs)*time.Millisecond, b.now())
	switch {
	case errors.Is(err, ErrIPCap):
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many identities from this network; try later"})
		return
	case errors.Is(err, ErrCooldown):
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "provisioning cooldown; retry shortly"})
		return
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "provision: " + err.Error()})
		return
	}
	key := DeriveKey(app.deriveSecret, byte(p.KeyVersion), caller)
	writeJSON(w, http.StatusOK, map[string]any{
		"key":     key,
		"credits": rec.Credits,
		"caller":  string(caller),
		"new":     rec.New,
	})
}

// serveBalance handles GET /<app>/_balance → the caller's credit balance.
func (b *Broker) serveBalance(w http.ResponseWriter, app *AppEntry, caller CallerID) {
	ps, ok := b.provStore()
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "store does not support provisioning"})
		return
	}
	c, err := ps.Credit(app.ID, string(caller))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "balance: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"credits": c, "caller": string(caller)})
}

// creditCost is how many credits a cloud method-path debits (default 1).
func creditCost(app *AppEntry, mpath string) int {
	if app.Provision != nil {
		if n, ok := app.Provision.CostCredits[mpath]; ok {
			return n
		}
	}
	return defaultCostCredits
}
