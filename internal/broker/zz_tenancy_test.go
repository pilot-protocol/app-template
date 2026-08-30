package broker

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// These tests are written as ATTACKS, not as feature checks: each asserts that a
// hostile caller CANNOT reach another tenant's resources. Isolation bugs do not
// show up on the happy path — a caller acting on someone else's resource looks
// exactly like a caller acting on their own unless something checks. So every
// case here is "Mallory tries X against Alice's resource and must fail", plus a
// paired assertion that Alice's own use still works.

// tenancyRegistry mirrors the shape of the real io.pilot.agentphone entry: a
// shared partner account, ownership tracked broker-side.
const tenancyRegistryJSON = `[{
  "id": "io.pilot.phone",
  "upstream": "%s",
  "key_env": "PHONE_KEY",
  "auth_header": "Authorization",
  "auth_scheme": "Bearer",
  "quota": 0,
  "allow": [
    "GET /v1/agents", "POST /v1/agents", "GET /v1/agents/{agent_id}",
    "DELETE /v1/agents/{agent_id}",
    "GET /v1/numbers", "POST /v1/numbers", "DELETE /v1/numbers/{number_id}",
    "GET /v1/numbers/{number_id}/messages",
    "POST /v1/messages", "GET /v1/calls"
  ],
  "credit": {"seed_credits": 5000000, "default_cost": 0,
             "cost_credits": {"POST /v1/numbers": 3000000, "POST /v1/messages": 20000}},
  "tenancy": {
    "param_types": {"agent_id": "agent", "number_id": "number"},
    "body_refs":   {"agent_id": "agent", "agentId": "agent", "number_id": "number", "phoneNumberId": "number"},
    "create": [
      {"method": "POST", "path": "/v1/agents",  "type": "agent",  "id_field": "id"},
      {"method": "POST", "path": "/v1/numbers", "type": "number", "id_field": "id"}
    ],
    "delete": [
      {"method": "DELETE", "path": "/v1/numbers/{number_id}", "type": "number", "param": "number_id"}
    ],
    "list": [
      {"method": "GET", "path": "/v1/agents",  "array": "data", "owner_by": [{"field": "id", "type": "agent"}]},
      {"method": "GET", "path": "/v1/numbers", "array": "data", "owner_by": [{"field": "id", "type": "number"}]},
      {"method": "GET", "path": "/v1/calls",   "array": "data",
       "owner_by": [{"field": "phoneNumberId", "type": "number"}, {"field": "agentId", "type": "agent"}]}
    ]
  }
}]`

// phoneUpstream fakes the partner: it answers for the WHOLE shared account,
// exactly as the real one does — which is why the broker must filter.
func phoneUpstream(t *testing.T, sent *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/agents":
			fmt.Fprintf(w, `{"id":"agent_%d","name":"a"}`, time.Now().UnixNano())
		case r.Method == "POST" && r.URL.Path == "/v1/numbers":
			fmt.Fprintf(w, `{"id":"num_%d","phoneNumber":"+1555%04d"}`, time.Now().UnixNano(), time.Now().UnixNano()%10000)
		case r.Method == "POST" && r.URL.Path == "/v1/messages":
			b := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(b)
			if sent != nil {
				*sent = append(*sent, string(b))
			}
			fmt.Fprint(w, `{"id":"msg_1","status":"sent"}`)
		case r.URL.Path == "/v1/agents":
			// the partner sees one shared account: EVERY tenant's agents
			fmt.Fprint(w, `{"data":[{"id":"agent_alice"},{"id":"agent_mallory"},{"id":"agent_legacy"}]}`)
		case r.URL.Path == "/v1/numbers":
			fmt.Fprint(w, `{"data":[{"id":"num_alice","phoneNumber":"+15550001"},{"id":"num_legacy","phoneNumber":"+15550002"}]}`)
		case r.URL.Path == "/v1/calls":
			fmt.Fprint(w, `{"data":[{"id":"call_1","phoneNumberId":"num_alice"},{"id":"call_2","phoneNumberId":"num_legacy"}]}`)
		default:
			fmt.Fprint(w, `{"ok":true}`)
		}
	}))
}

func tenancyBroker(t *testing.T, sent *[]string) (*Broker, func()) {
	t.Helper()
	up := phoneUpstream(t, sent)
	reg, err := ParseRegistry([]byte(fmt.Sprintf(tenancyRegistryJSON, up.URL)),
		func(string) string { return "master-key" })
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	b := New(reg, NewMemStore())
	b.Verify = VerifyConfig{Window: time.Hour}
	return b, up.Close
}

func do(t *testing.T, b *Broker, priv ed25519.PrivateKey, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, signedReq(t, priv, method, path, body, time.Now()))
	return rec
}

// TestTenancy_CannotSendFromAnotherTenantsAgent: Mallory names Alice's agent in
// the BODY of a send. The path is entirely her own, so only a body-level
// ownership check can stop this. It must never reach the partner.
func TestTenancy_CannotSendFromAnotherTenantsAgent(t *testing.T) {
	var sent []string
	b, done := tenancyBroker(t, &sent)
	defer done()

	_, alice := newKey(t)
	_, mallory := newKey(t)

	// Alice creates an agent → she owns it.
	rec := do(t, b, alice, "POST", "/io.pilot.phone/v1/agents", []byte(`{"name":"alice"}`))
	if rec.Code != 200 {
		t.Fatalf("alice create agent: %d %s", rec.Code, rec.Body)
	}
	var created struct{ ID string }
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	// Mallory names Alice's agent in her own request body.
	body := []byte(fmt.Sprintf(`{"agent_id":%q,"to_number":"+15551234","body":"spam"}`, created.ID))
	rec = do(t, b, mallory, "POST", "/io.pilot.phone/v1/messages", body)
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant send: status %d, want 404 — mallory acted through alice's agent", rec.Code)
	}
	if len(sent) != 0 {
		t.Errorf("cross-tenant send REACHED THE PARTNER: %v", sent)
	}

	// Alice sending from her own agent still works — isolation must not break the product.
	rec = do(t, b, alice, "POST", "/io.pilot.phone/v1/messages", body)
	if rec.Code != 200 {
		t.Errorf("alice's own send: %d %s, want 200", rec.Code, rec.Body)
	}
	if len(sent) != 1 {
		t.Errorf("alice's send should have reached the partner exactly once, got %d", len(sent))
	}
}

// TestTenancy_LegacyResourcesUnreachable: resources that predate the ledger are
// owned by nobody. Deny-by-default must make them unreachable to EVERYONE rather
// than unclaimed and therefore available to the first caller who names one.
func TestTenancy_LegacyResourcesUnreachable(t *testing.T) {
	var sent []string
	b, done := tenancyBroker(t, &sent)
	defer done()
	_, mallory := newKey(t)

	body := []byte(`{"agent_id":"agent_legacy","to_number":"+15551234","body":"spam"}`)
	if rec := do(t, b, mallory, "POST", "/io.pilot.phone/v1/messages", body); rec.Code != 404 {
		t.Errorf("legacy agent send: %d, want 404", rec.Code)
	}
	if rec := do(t, b, mallory, "GET", "/io.pilot.phone/v1/numbers/num_legacy/messages", nil); rec.Code != 404 {
		t.Errorf("legacy number read: %d, want 404", rec.Code)
	}
	// And it must not be claimable by simply asserting it.
	if rec := do(t, b, mallory, "DELETE", "/io.pilot.phone/v1/numbers/num_legacy", nil); rec.Code != 404 {
		t.Errorf("legacy number delete: %d, want 404", rec.Code)
	}
	if len(sent) != 0 {
		t.Errorf("legacy resource reached partner: %v", sent)
	}
}

// TestTenancy_ListsAreFilteredToOwner: a pilot user must not see any number but
// their own. The partner answers for the whole shared account, so the broker has
// to strip the answer down to the caller's rows before returning it.
func TestTenancy_ListsAreFilteredToOwner(t *testing.T) {
	b, done := tenancyBroker(t, nil)
	defer done()
	_, alice := newKey(t)
	_, mallory := newKey(t)

	// Nobody owns anything yet → every list is empty, NOT the account-wide answer.
	rec := do(t, b, mallory, "GET", "/io.pilot.phone/v1/numbers", nil)
	if n := countData(t, rec.Body.Bytes()); n != 0 {
		t.Errorf("unowned caller saw %d numbers, want 0 — body: %s", n, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "+15550002") {
		t.Errorf("LEAK: another tenant's number was disclosed: %s", rec.Body)
	}

	// Alice owns num_alice → she sees exactly one, and never num_legacy.
	if err := b.ownerStore().Claim("io.pilot.phone", "number", "num_alice", callerOf(t, alice), time.Now()); err != nil {
		t.Fatal(err)
	}
	rec = do(t, b, alice, "GET", "/io.pilot.phone/v1/numbers", nil)
	if n := countData(t, rec.Body.Bytes()); n != 1 {
		t.Errorf("alice saw %d numbers, want 1 — body: %s", n, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "num_legacy") {
		t.Errorf("LEAK: alice saw another tenant's number: %s", rec.Body)
	}
	// Mallory still sees nothing.
	rec = do(t, b, mallory, "GET", "/io.pilot.phone/v1/numbers", nil)
	if n := countData(t, rec.Body.Bytes()); n != 0 {
		t.Errorf("mallory saw %d numbers, want 0", n)
	}
}

// TestTenancy_InboundResourcesAttributedByLink: an inbound call is created by
// the partner, so the tenant never "created" it. It must still be visible to the
// number's owner (via owner_by link) and invisible to everyone else.
func TestTenancy_InboundCallsFollowNumberOwnership(t *testing.T) {
	b, done := tenancyBroker(t, nil)
	defer done()
	_, alice := newKey(t)
	_, mallory := newKey(t)
	if err := b.ownerStore().Claim("io.pilot.phone", "number", "num_alice", callerOf(t, alice), time.Now()); err != nil {
		t.Fatal(err)
	}

	rec := do(t, b, alice, "GET", "/io.pilot.phone/v1/calls", nil)
	if n := countData(t, rec.Body.Bytes()); n != 1 {
		t.Errorf("alice saw %d calls, want 1 (her inbound call) — %s", n, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "call_2") {
		t.Errorf("LEAK: alice saw a call on another tenant's number: %s", rec.Body)
	}
	rec = do(t, b, mallory, "GET", "/io.pilot.phone/v1/calls", nil)
	if n := countData(t, rec.Body.Bytes()); n != 0 {
		t.Errorf("mallory saw %d calls, want 0", n)
	}
}

// TestTenancy_NestedBodyRefIsChecked: wrapping the stolen id one level deep must
// not bypass the check. A top-level-only scan would be trivially defeated.
func TestTenancy_NestedBodyRefIsChecked(t *testing.T) {
	var sent []string
	b, done := tenancyBroker(t, &sent)
	defer done()
	_, alice := newKey(t)
	_, mallory := newKey(t)

	rec := do(t, b, alice, "POST", "/io.pilot.phone/v1/agents", []byte(`{"name":"alice"}`))
	var created struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	for _, body := range []string{
		fmt.Sprintf(`{"message":{"agent_id":%q},"to_number":"+1555"}`, created.ID),      // nested object
		fmt.Sprintf(`{"batch":[{"agent_id":%q}],"to_number":"+1555"}`, created.ID),      // inside an array
		fmt.Sprintf(`{"a":{"b":{"c":{"agentId":%q}}},"to_number":"+1555"}`, created.ID), // deep + camelCase alias
	} {
		rec := do(t, b, mallory, "POST", "/io.pilot.phone/v1/messages", []byte(body))
		if rec.Code != 404 {
			t.Errorf("nested ref %s: status %d, want 404", body, rec.Code)
		}
	}
	if len(sent) != 0 {
		t.Errorf("a nested cross-tenant ref reached the partner: %v", sent)
	}
}

// TestTenancy_OwnershipIsNotTransferable: a second caller claiming a resource
// that already has an owner must lose. Otherwise "claim" is a takeover.
func TestTenancy_OwnershipIsNotTransferable(t *testing.T) {
	s := NewMemStore()
	now := time.Now()
	if err := s.Claim("app", "number", "num_1", "alice", now); err != nil {
		t.Fatalf("alice claim: %v", err)
	}
	if err := s.Claim("app", "number", "num_1", "mallory", now); err != ErrOwned {
		t.Errorf("mallory claim: %v, want ErrOwned", err)
	}
	if !Owns(s, "app", "number", "num_1", "alice") {
		t.Error("alice must still own num_1")
	}
	if Owns(s, "app", "number", "num_1", "mallory") {
		t.Error("mallory must NOT own num_1")
	}
	// Re-claim by the true owner stays idempotent.
	if err := s.Claim("app", "number", "num_1", "alice", now); err != nil {
		t.Errorf("idempotent re-claim: %v", err)
	}
}

// TestTenancy_TypeConfusion: an id owned as one type must not authorize the same
// id used as another type.
func TestTenancy_TypeConfusion(t *testing.T) {
	s := NewMemStore()
	_ = s.Claim("app", "agent", "x_1", "alice", time.Now())
	if Owns(s, "app", "number", "x_1", "alice") {
		t.Error("owning agent x_1 must not confer ownership of number x_1")
	}
}

// TestTenancy_FailsClosedWithoutLedger: tenancy declared but the store cannot
// record ownership → deny, never serve unisolated.
func TestTenancy_FailsClosedWithoutLedger(t *testing.T) {
	tn := &Tenancy{ParamTypes: map[string]string{"agent_id": "agent"}}
	tn.compile()
	if _, ok := tn.EnforceRequest(nil, "app", nil, "GET", "/v1/agents/a1", "", "", nil, "alice"); ok {
		t.Error("nil ledger must deny")
	}
	if _, ok := tn.EnforceRequest(NewMemStore(), "app", nil, "GET", "/v1/agents/a1", "", "", nil, ""); ok {
		t.Error("empty caller must deny")
	}
}

// TestTenancy_MalformedBodyDenied: a body that cannot be parsed cannot be
// checked, so it must not be forwarded.
func TestTenancy_MalformedBodyDenied(t *testing.T) {
	var sent []string
	b, done := tenancyBroker(t, &sent)
	defer done()
	_, mallory := newKey(t)
	rec := do(t, b, mallory, "POST", "/io.pilot.phone/v1/messages", []byte(`{"agent_id": broken`))
	if rec.Code != 404 {
		t.Errorf("malformed body: %d, want 404", rec.Code)
	}
	if len(sent) != 0 {
		t.Errorf("malformed body reached partner: %v", sent)
	}
}

// countData counts elements of the "data" array in a JSON body.
func countData(t *testing.T, b []byte) int {
	t.Helper()
	var v struct {
		Data []any `json:"data"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	return len(v.Data)
}

// callerOf renders a private key's identity the way the broker records it.
func callerOf(t *testing.T, priv ed25519.PrivateKey) string {
	t.Helper()
	return base64.RawStdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
}

// TestTenancy_DuplicateKeyBypassDenied: the broker validates the body with one
// parser and forwards raw bytes for the partner to re-parse with another. When a
// key repeats, the two parsers can disagree about its value, and the broker would
// authorize a value the partner never acts on. Such a body must be refused.
func TestTenancy_DuplicateKeyBypassDenied(t *testing.T) {
	var sent []string
	b, done := tenancyBroker(t, &sent)
	defer done()
	_, alice := newKey(t)
	_, mallory := newKey(t)

	rec := do(t, b, alice, "POST", "/io.pilot.phone/v1/agents", []byte(`{"name":"alice"}`))
	var created struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// Mallory owns an agent of her own, and names Alice's FIRST.
	rec = do(t, b, mallory, "POST", "/io.pilot.phone/v1/agents", []byte(`{"name":"mallory"}`))
	var mine struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &mine)

	body := []byte(fmt.Sprintf(`{"agent_id":%q,"agent_id":%q,"to_number":"+1555","body":"spam"}`, created.ID, mine.ID))
	rec = do(t, b, mallory, "POST", "/io.pilot.phone/v1/messages", body)
	if rec.Code != 404 {
		t.Errorf("duplicate-key bypass: status %d, want 404", rec.Code)
	}
	if len(sent) != 0 {
		t.Errorf("duplicate-key body REACHED THE PARTNER: %v", sent)
	}
}

// TestTenancy_CountsDoNotLeakUnfilteredSet: filtering the array is not enough.
// A sibling count left at the partner's value still reports the size of the
// whole shared account, so a caller who can see none of the rows can still read
// how many exist — and watch that number move as other tenants work.
func TestTenancy_CountsDoNotLeakUnfilteredSet(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"num_alice"},{"id":"num_other"},{"id":"num_third"}],"total":3,"hasMore":true}`)
	}))
	defer up.Close()
	reg, err := ParseRegistry([]byte(`[{
	  "id":"io.pilot.phone","upstream":"`+up.URL+`","key_env":"K","auth_header":"Authorization","auth_scheme":"Bearer",
	  "allow":["GET /v1/numbers"],
	  "tenancy":{"param_types":{"number_id":"number"},
	    "create":[{"method":"POST","path":"/v1/numbers","type":"number","id_field":"id"}],
	    "list":[{"method":"GET","path":"/v1/numbers","array":"data",
	             "owner_by":[{"field":"id","type":"number"}],"claim_as":"number",
	             "count_fields":["total"]}]}}]`), func(string) string { return "k" })
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	b := New(reg, NewMemStore())
	b.Verify = VerifyConfig{Window: time.Hour}
	_, mallory := newKey(t)

	rec := do(t, b, mallory, "GET", "/io.pilot.phone/v1/numbers", nil)
	var got struct {
		Data  []any       `json:"data"`
		Total json.Number `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %s: %v", rec.Body, err)
	}
	if len(got.Data) != 0 {
		t.Errorf("data = %d rows, want 0", len(got.Data))
	}
	if got.Total.String() != "0" {
		t.Errorf("total = %s, want 0 — the account-wide count leaked", got.Total)
	}
}
