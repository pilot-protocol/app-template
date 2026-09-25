package broker

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A literal allow entry next to a templated sibling (GET /v1/agents/voices beside
// GET /v1/agents/{agent_id}) is a static route. Tenancy must not bind the literal
// segment as a resource id: doing so refused io.pilot.agentphone's list_voices
// with a 404 for every caller.
func TestTenancy_LiteralRouteBesideTemplateIsNotAnID(t *testing.T) {
	var hits []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"voice_1"}]}`)
	}))
	defer up.Close()
	regJSON := strings.Replace(fmt.Sprintf(tenancyRegistryJSON, up.URL),
		`"GET /v1/agents/{agent_id}",`, `"GET /v1/agents/{agent_id}", "GET /v1/agents/voices",`, 1)
	reg, err := ParseRegistry([]byte(regJSON), func(string) string { return "master-key" })
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	b := New(reg, NewMemStore())
	b.Verify = VerifyConfig{Window: time.Hour}
	_, mallory := newKey(t)

	if rec := do(t, b, mallory, "GET", "/io.pilot.phone/v1/agents/voices", nil); rec.Code != 200 {
		t.Fatalf("literal route: %d %s, want 200", rec.Code, rec.Body)
	}
	if len(hits) != 1 || hits[0] != "GET /v1/agents/voices" {
		t.Fatalf("literal route should reach the partner once, got %v", hits)
	}

	// The template itself is still ownership-checked: an id Mallory does not own
	// is refused and never forwarded.
	if rec := do(t, b, mallory, "GET", "/io.pilot.phone/v1/agents/agent_alice", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unowned agent read: %d, want 404", rec.Code)
	}
	if len(hits) != 1 {
		t.Errorf("unowned agent read reached the partner: %v", hits)
	}
}
