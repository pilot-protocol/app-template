package broker

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Isolation tests for the two managed apps added in the io.pilot.kinetic /
// io.pilot.rentahuman submissions. Both front a SHARED partner account: one key
// for every Pilot user. The partner therefore answers for the whole account and
// cannot tell tenants apart, so the broker is the only isolation boundary.
//
// Written as attacks, following zz_tenancy_test.go: each case is "Mallory tries
// to reach Alice's resource and must fail", with a paired assertion that Alice's
// own access still works. A pure feature test would pass even with tenancy off,
// because acting on someone else's resource looks identical to acting on your own.

// ── io.pilot.kinetic ────────────────────────────────────────────────────────
//
// Note param_types: the broker's map is keyed by PARAM NAME globally, so studies,
// teardowns and tasks each need a distinct placeholder. If all three used {id}
// they would collapse into one resource type and a task id would be checked as a
// study — denying every legitimate task read.
const kineticRegistryJSON = `[{
  "id": "io.pilot.kinetic",
  "upstream": "%s",
  "key_env": "KINETIC_MASTER_KEY",
  "auth_header": "Authorization",
  "auth_scheme": "Bearer",
  "quota": 0,
  "allow": [
    "GET /v1/methods", "POST /v1/methods/recommendation",
    "GET /v1/studies", "POST /v1/studies", "POST /v1/studies/prefill",
    "GET /v1/studies/{study_id}", "PATCH /v1/studies/{study_id}",
    "POST /v1/studies/{study_id}/duplicate",
    "GET /v1/studies/{study_id}/results",
    "POST /v1/studies/{study_id}/launch",
    "GET /v1/billing/offers", "POST /v1/billing/checkouts/study",
    "POST /v1/teardowns", "GET /v1/teardowns/{teardown_id}",
    "GET /v1/research", "GET /v1/tasks/{task_id}"
  ],
  "credit": {"seed_credits": 5000000, "default_cost": 0, "max_identities_per_ip": 20},
  "tenancy": {
    "param_types": {"study_id": "study", "teardown_id": "teardown"},
    "body_refs":   {"study_id": "study"},
    "create": [
      {"method": "POST", "path": "/v1/studies", "type": "study", "id_field": "id"},
      {"method": "POST", "path": "/v1/studies/prefill", "type": "study", "id_field": "id"},
      {"method": "POST", "path": "/v1/studies/{study_id}/duplicate", "type": "study", "id_field": "id"},
      {"method": "POST", "path": "/v1/teardowns", "type": "teardown", "id_field": "id"}
    ],
    "list": [
      {"method": "GET", "path": "/v1/studies", "array": "items",
       "owner_by": [{"field": "id", "type": "study"}], "count_fields": ["total"]}
    ]
  }
}]`

// kineticUpstream answers for the WHOLE shared account, as the real API does.
func kineticUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/studies":
			fmt.Fprintf(w, `{"id":"stu_%d","state":"draft"}`, time.Now().UnixNano())
		case r.Method == "POST" && r.URL.Path == "/v1/teardowns":
			fmt.Fprintf(w, `{"id":"tdn_%d"}`, time.Now().UnixNano())
		case r.URL.Path == "/v1/studies":
			// every tenant's studies, plus one predating the ledger
			fmt.Fprint(w, `{"items":[{"id":"stu_alice"},{"id":"stu_mallory"},{"id":"stu_legacy"}],"total":3}`)
		case r.URL.Path == "/v1/methods":
			fmt.Fprint(w, `{"methods":[{"study_type":"van_westendorp","price_cents":14900}]}`)
		default:
			// a study/teardown/task fetch: the partner happily serves any id
			fmt.Fprintf(w, `{"ok":true,"path":%q}`, r.URL.Path)
		}
	}))
}

func kineticBroker(t *testing.T) (*Broker, func()) {
	t.Helper()
	up := kineticUpstream(t)
	reg, err := ParseRegistry([]byte(fmt.Sprintf(kineticRegistryJSON, up.URL)),
		func(string) string { return "master-key" })
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	b := New(reg, NewMemStore())
	b.Verify = VerifyConfig{Window: time.Hour}
	return b, up.Close
}

// createStudy returns the id of a study created BY this caller.
func createStudy(t *testing.T, b *Broker, priv ed25519.PrivateKey) string {
	t.Helper()
	rec := do(t, b, priv, "POST", "/io.pilot.kinetic/v1/studies", []byte(`{"study_type":"van_westendorp"}`))
	if rec.Code != 200 {
		t.Fatalf("create study: got %d, body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.ID == "" {
		t.Fatalf("create study: no id in %s", rec.Body.String())
	}
	return out.ID
}

// TestKinetic_CannotReadAnotherTenantsStudy is the central risk: results of a
// pricing study are commercially sensitive, and on a shared account every study
// is one GET away unless the broker checks ownership.
func TestKinetic_CannotReadAnotherTenantsStudy(t *testing.T) {
	b, done := kineticBroker(t)
	defer done()
	_, alice := newKey(t)
	_, mallory := newKey(t)

	id := createStudy(t, b, alice)

	// Alice reads her own study and its results.
	for _, p := range []string{"/io.pilot.kinetic/v1/studies/" + id, "/io.pilot.kinetic/v1/studies/" + id + "/results"} {
		if rec := do(t, b, alice, "GET", p, nil); rec.Code != 200 {
			t.Fatalf("alice %s: got %d, want 200", p, rec.Code)
		}
	}

	// Mallory must not, and must not be able to tell it exists.
	for _, p := range []string{"/io.pilot.kinetic/v1/studies/" + id, "/io.pilot.kinetic/v1/studies/" + id + "/results"} {
		rec := do(t, b, mallory, "GET", p, nil)
		if rec.Code != 404 {
			t.Fatalf("mallory %s: got %d, want 404 (leak)", p, rec.Code)
		}
	}
}

// TestKinetic_CannotMutateOrLaunchAnotherTenantsStudy: reads are not the only
// risk. Launching spends money and freezes the questions; editing corrupts a
// study someone else is about to pay for.
func TestKinetic_CannotMutateOrLaunchAnotherTenantsStudy(t *testing.T) {
	b, done := kineticBroker(t)
	defer done()
	_, alice := newKey(t)
	_, mallory := newKey(t)
	id := createStudy(t, b, alice)

	cases := []struct {
		method, path string
		body         []byte
	}{
		{"PATCH", "/io.pilot.kinetic/v1/studies/" + id, []byte(`{"name":"pwned"}`)},
		{"POST", "/io.pilot.kinetic/v1/studies/" + id + "/launch", []byte(`{}`)},
		{"POST", "/io.pilot.kinetic/v1/studies/" + id + "/duplicate", []byte(`{}`)},
	}
	for _, c := range cases {
		if rec := do(t, b, mallory, c.method, c.path, c.body); rec.Code != 404 {
			t.Fatalf("mallory %s %s: got %d, want 404", c.method, c.path, rec.Code)
		}
	}
}

// TestKinetic_CheckoutCannotTargetAnotherTenantsStudy: study_id arrives in the
// BODY here, not the path, so a path-only check would miss it entirely.
func TestKinetic_CheckoutCannotTargetAnotherTenantsStudy(t *testing.T) {
	b, done := kineticBroker(t)
	defer done()
	_, alice := newKey(t)
	_, mallory := newKey(t)
	id := createStudy(t, b, alice)

	body := []byte(fmt.Sprintf(`{"study_id":%q}`, id))
	if rec := do(t, b, mallory, "POST", "/io.pilot.kinetic/v1/billing/checkouts/study", body); rec.Code != 404 {
		t.Fatalf("mallory checkout on alice's study: got %d, want 404 (body_refs not enforced)", rec.Code)
	}
	if rec := do(t, b, alice, "POST", "/io.pilot.kinetic/v1/billing/checkouts/study", body); rec.Code != 200 {
		t.Fatalf("alice checkout on her own study: got %d, want 200", rec.Code)
	}
}

// TestKinetic_StudyListIsFilteredAndCountRecomputed: filtering the array is not
// enough on its own — a `total` left at the partner's value still reports how
// many studies exist across every tenant.
func TestKinetic_StudyListIsFilteredAndCountRecomputed(t *testing.T) {
	b, done := kineticBroker(t)
	defer done()
	_, mallory := newKey(t)

	rec := do(t, b, mallory, "GET", "/io.pilot.kinetic/v1/studies", nil)
	if rec.Code != 200 {
		t.Fatalf("list: got %d", rec.Code)
	}
	var out struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(out.Items) != 0 {
		t.Fatalf("mallory owns nothing but saw %d studies: %s", len(out.Items), rec.Body.String())
	}
	if out.Total != 0 {
		t.Fatalf("total leaked the shared account: got %d, want 0", out.Total)
	}
}

// TestKinetic_ResourceKindsDoNotCollide guards the param-name trap: a task id
// must not be ownership-checked as a study. If {id} were reused across kinds the
// map would collapse and this read would 404 for a legitimate owner.
func TestKinetic_ResourceKindsDoNotCollide(t *testing.T) {
	b, done := kineticBroker(t)
	defer done()
	_, alice := newKey(t)

	rec := do(t, b, alice, "POST", "/io.pilot.kinetic/v1/teardowns", []byte(`{"url":"https://x.example"}`))
	if rec.Code != 200 {
		t.Fatalf("create teardown: got %d", rec.Code)
	}
	var td struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &td)
	if rec := do(t, b, alice, "GET", "/io.pilot.kinetic/v1/teardowns/"+td.ID, nil); rec.Code != 200 {
		t.Fatalf("alice reading her own teardown: got %d, want 200", rec.Code)
	}
	// Public, unowned surfaces stay reachable by everyone.
	if rec := doNewCaller(t, b, "GET", "/io.pilot.kinetic/v1/methods"); rec.Code != 200 {
		t.Fatalf("public /v1/methods: got %d, want 200", rec.Code)
	}
}

// ── io.pilot.rentahuman ─────────────────────────────────────────────────────
//
// The Partner API already 404s another PARTNER's ids, but every Pilot user is
// the same partner, so that check does nothing between tenants.
const rentahumanRegistryJSON = `[{
  "id": "io.pilot.rentahuman",
  "upstream": "%s",
  "key_env": "RENTAHUMAN_MASTER_KEY",
  "auth_header": "X-API-Key",
  "quota": 0,
  "allow": [
    "POST /api/partner/v1/requests", "GET /api/partner/v1/requests",
    "GET /api/partner/v1/requests/{requestId}",
    "POST /api/partner/v1/requests/{requestId}/messages",
    "GET /api/partner/v1/bounties"
  ],
  "credit": {"seed_credits": 5000000, "default_cost": 0, "max_identities_per_ip": 20},
  "tenancy": {
    "param_types": {"requestId": "request"},
    "create": [
      {"method": "POST", "path": "/api/partner/v1/requests", "type": "request", "id_field": "requestId"}
    ],
    "list": [
      {"method": "GET", "path": "/api/partner/v1/requests", "array": "requests",
       "owner_by": [{"field": "requestId", "type": "request"}]}
    ]
  }
}]`

func rentahumanUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/partner/v1/requests":
			fmt.Fprintf(w, `{"success":true,"requestId":"req_%d","status":"received"}`, time.Now().UnixNano())
		case r.URL.Path == "/api/partner/v1/requests":
			fmt.Fprint(w, `{"success":true,"requests":[{"requestId":"req_alice","requesterPhoneNumber":"+14155550100"},{"requestId":"req_mallory"}],"nextCursor":null}`)
		default:
			fmt.Fprintf(w, `{"success":true,"path":%q}`, r.URL.Path)
		}
	}))
}

func rentahumanBroker(t *testing.T) (*Broker, func()) {
	t.Helper()
	up := rentahumanUpstream(t)
	reg, err := ParseRegistry([]byte(fmt.Sprintf(rentahumanRegistryJSON, up.URL)),
		func(string) string { return "master-key" })
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	b := New(reg, NewMemStore())
	b.Verify = VerifyConfig{Window: time.Hour}
	return b, up.Close
}

// TestRentAHuman_CannotReadAnotherTenantsRequest: a request thread carries the
// task, the location and the requester's phone number, so a leak here is
// customer PII, not just metadata.
func TestRentAHuman_CannotReadAnotherTenantsRequest(t *testing.T) {
	b, done := rentahumanBroker(t)
	defer done()
	_, alice := newKey(t)
	_, mallory := newKey(t)

	rec := do(t, b, alice, "POST", "/io.pilot.rentahuman/api/partner/v1/requests",
		[]byte(`{"task":"clean","externalChatId":"c1","requesterPhoneNumber":"+14155550100","budgetUsd":150}`))
	if rec.Code != 200 {
		t.Fatalf("create: got %d, %s", rec.Code, rec.Body.String())
	}
	var out struct {
		RequestID string `json:"requestId"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.RequestID == "" {
		t.Fatalf("no requestId in %s", rec.Body.String())
	}

	if rec := do(t, b, alice, "GET", "/io.pilot.rentahuman/api/partner/v1/requests/"+out.RequestID, nil); rec.Code != 200 {
		t.Fatalf("alice reading her own request: got %d, want 200", rec.Code)
	}
	if rec := do(t, b, mallory, "GET", "/io.pilot.rentahuman/api/partner/v1/requests/"+out.RequestID, nil); rec.Code != 404 {
		t.Fatalf("mallory reading alice's request: got %d, want 404 (PII leak)", rec.Code)
	}
}

// TestRentAHuman_CannotPostIntoAnotherTenantsThread: writing into someone else's
// ops thread would put words in their mouth to a human coordinator.
func TestRentAHuman_CannotPostIntoAnotherTenantsThread(t *testing.T) {
	b, done := rentahumanBroker(t)
	defer done()
	_, alice := newKey(t)
	_, mallory := newKey(t)

	rec := do(t, b, alice, "POST", "/io.pilot.rentahuman/api/partner/v1/requests",
		[]byte(`{"task":"clean","externalChatId":"c1","requesterPhoneNumber":"+14155550100","budgetUsd":150}`))
	var out struct {
		RequestID string `json:"requestId"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)

	p := "/io.pilot.rentahuman/api/partner/v1/requests/" + out.RequestID + "/messages"
	if rec := do(t, b, mallory, "POST", p, []byte(`{"message":"cancel it"}`)); rec.Code != 404 {
		t.Fatalf("mallory posting into alice's thread: got %d, want 404", rec.Code)
	}
	if rec := do(t, b, alice, "POST", p, []byte(`{"message":"confirmed"}`)); rec.Code != 200 {
		t.Fatalf("alice posting into her own thread: got %d, want 200", rec.Code)
	}
}

// TestRentAHuman_RequestListIsFiltered: the unfiltered list carries other
// tenants' phone numbers.
func TestRentAHuman_RequestListIsFiltered(t *testing.T) {
	b, done := rentahumanBroker(t)
	defer done()
	_, mallory := newKey(t)

	rec := do(t, b, mallory, "GET", "/io.pilot.rentahuman/api/partner/v1/requests", nil)
	if rec.Code != 200 {
		t.Fatalf("list: got %d", rec.Code)
	}
	var out struct {
		Requests []struct {
			RequestID string `json:"requestId"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Requests) != 0 {
		t.Fatalf("mallory owns nothing but saw %d requests: %s", len(out.Requests), rec.Body.String())
	}
}

// doNewCaller issues a request as a caller seen for the first time.
func doNewCaller(t *testing.T, b *Broker, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	_, k := newKey(t)
	return do(t, b, k, method, path, nil)
}
