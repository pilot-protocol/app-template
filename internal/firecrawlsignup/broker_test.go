package firecrawlsignup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakePartner stands in for integrations.firecrawl.dev. It records what it saw
// so the tests can assert on the exact request we send Firecrawl.
type fakePartner struct {
	srv     *httptest.Server
	calls   int
	emails  []string
	authSaw []string
	byEmail map[string]string // email -> issued key (models "one team per email")
}

func newFakePartner(t *testing.T) *fakePartner {
	t.Helper()
	f := &fakePartner{byEmail: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/partner/v1/accounts" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.calls++
		f.authSaw = append(f.authSaw, r.Header.Get("Authorization"))
		var in struct {
			Email string `json:"email"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.emails = append(f.emails, in.Email)
		key, existed := f.byEmail[in.Email]
		if !existed {
			key = "fc-" + strings.ReplaceAll(in.Email, "@", "-")
			f.byEmail[in.Email] = key
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"apiKey": key, "alreadyExisted": existed})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newTestBroker(t *testing.T, f *fakePartner, cfg Config) *Broker {
	t.Helper()
	if cfg.PartnerKey == "" {
		cfg.PartnerKey = "partner-secret-do-not-leak"
	}
	if cfg.MailDomain == "" {
		cfg.MailDomain = "agents.pilotprotocol.network"
	}
	cfg.PartnerAPI = f.srv.URL
	cfg.TermsRev = "2026-08"
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

// A caller gets a real key, a derived mailbox, and a recorded ToS acceptance.
func TestSignupMintsAccountAndRecordsConsent(t *testing.T) {
	f := newFakePartner(t)
	b := newTestBroker(t, f, Config{})

	acct, err := b.Signup(context.Background(), "caller-alpha", "1.2.3.4")
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}
	if !strings.HasPrefix(acct.APIKey, "fc-") {
		t.Errorf("api key = %q, want an fc- key", acct.APIKey)
	}
	if !strings.HasSuffix(acct.Email, "@agents.pilotprotocol.network") {
		t.Errorf("email = %q, want a mailbox on the configured domain", acct.Email)
	}
	if !strings.HasPrefix(acct.Email, "pilot-") {
		t.Errorf("email = %q, want the pilot- prefix", acct.Email)
	}
	if acct.TermsURL != DefaultTermsURL {
		t.Errorf("terms url = %q, want %q", acct.TermsURL, DefaultTermsURL)
	}
	// The acceptance timestamp is the artefact we would show Firecrawl.
	if _, err := time.Parse(time.RFC3339, acct.AcceptedAt); err != nil {
		t.Errorf("terms_accepted_at = %q, not RFC3339: %v", acct.AcceptedAt, err)
	}
	if acct.Cached {
		t.Error("first signup reported cached")
	}
}

// The mailbox must be a pure function of the caller: stable across calls, and
// different per caller. If it were not, a reinstall would orphan the account.
func TestMailboxIsDerivedAndStable(t *testing.T) {
	f := newFakePartner(t)
	b := newTestBroker(t, f, Config{})
	a1 := b.mailboxFor("caller-alpha")
	a2 := b.mailboxFor("caller-alpha")
	other := b.mailboxFor("caller-beta")
	if a1 != a2 {
		t.Errorf("mailbox not stable: %q vs %q", a1, a2)
	}
	if a1 == other {
		t.Error("two callers derived the same mailbox")
	}
	if strings.Contains(a1, "caller-alpha") {
		t.Errorf("mailbox %q leaks the raw caller id", a1)
	}
}

// A repeat signup must return the SAME account and must not mint again —
// otherwise a reinstall would strand the user's credits on a dead account.
func TestSignupIsIdempotentPerCaller(t *testing.T) {
	f := newFakePartner(t)
	b := newTestBroker(t, f, Config{})

	first, err := b.Signup(context.Background(), "caller-alpha", "1.2.3.4")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := b.Signup(context.Background(), "caller-alpha", "1.2.3.4")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.APIKey != second.APIKey || first.Email != second.Email {
		t.Errorf("repeat signup returned a different account: %+v vs %+v", first, second)
	}
	if !second.Cached {
		t.Error("repeat signup should report cached")
	}
	if f.calls != 1 {
		t.Errorf("partner API called %d times, want exactly 1", f.calls)
	}
}

// Two different callers get two different accounts — this is the whole point of
// moving off the shared key.
func TestDistinctCallersGetDistinctAccounts(t *testing.T) {
	f := newFakePartner(t)
	b := newTestBroker(t, f, Config{})
	a, _ := b.Signup(context.Background(), "caller-alpha", "1.2.3.4")
	c, _ := b.Signup(context.Background(), "caller-beta", "1.2.3.4")
	if a.APIKey == c.APIKey {
		t.Error("two callers share an api key")
	}
	if a.Email == c.Email {
		t.Error("two callers share a mailbox")
	}
}

// The partner key authenticates us to Firecrawl and must never reach the caller.
func TestPartnerKeyIsSentUpstreamAndNeverReturned(t *testing.T) {
	f := newFakePartner(t)
	b := newTestBroker(t, f, Config{})
	acct, err := b.Signup(context.Background(), "caller-alpha", "1.2.3.4")
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}
	if len(f.authSaw) != 1 || f.authSaw[0] != "Bearer partner-secret-do-not-leak" {
		t.Errorf("upstream Authorization = %v, want the partner key", f.authSaw)
	}
	blob, _ := json.Marshal(acct)
	if strings.Contains(string(blob), "partner-secret-do-not-leak") {
		t.Errorf("partner key leaked to the caller: %s", blob)
	}
}

// A partner-API failure must not persist a half-made account, or the caller
// would be permanently stuck with a cached row holding no usable key.
func TestPartnerFailureLeavesNoAccount(t *testing.T) {
	f := newFakePartner(t)
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":"partner_unauthorized"}}`, http.StatusUnauthorized)
	})
	b := newTestBroker(t, f, Config{})
	if _, err := b.Signup(context.Background(), "caller-alpha", "1.2.3.4"); err == nil {
		t.Fatal("expected an error when the partner API rejects us")
	}
	if _, ok, _ := b.store.get("caller-alpha"); ok {
		t.Error("a failed mint persisted an account row")
	}
}

// The per-IP cap stops one host farming accounts.
func TestPerIPIdentityCap(t *testing.T) {
	f := newFakePartner(t)
	b := newTestBroker(t, f, Config{MaxIdentitiesPerIP: 2})
	for _, c := range []string{"c1", "c2"} {
		if _, err := b.Signup(context.Background(), c, "9.9.9.9"); err != nil {
			t.Fatalf("signup %s: %v", c, err)
		}
	}
	if _, err := b.Signup(context.Background(), "c3", "9.9.9.9"); err == nil {
		t.Error("third distinct caller from one IP should be refused")
	}
	// A caller already known to that IP is still allowed through the cap.
	if _, err := b.Signup(context.Background(), "c1", "9.9.9.9"); err != nil {
		t.Errorf("existing caller refused by the cap: %v", err)
	}
}

// The stored key must not be readable straight out of the DB file.
func TestAPIKeyIsSealedAtRest(t *testing.T) {
	f := newFakePartner(t)
	b := newTestBroker(t, f, Config{EncKeyHex: strings.Repeat("ab", 32)})
	if _, err := b.Signup(context.Background(), "caller-alpha", "1.2.3.4"); err != nil {
		t.Fatalf("Signup: %v", err)
	}
	var sealed string
	if err := b.store.db.QueryRow(`SELECT api_key FROM accounts WHERE caller = ?`, "caller-alpha").Scan(&sealed); err != nil {
		t.Fatalf("read raw row: %v", err)
	}
	if strings.HasPrefix(sealed, "fc-") {
		t.Errorf("api key stored in the clear: %q", sealed)
	}
	// ...but the ToS acceptance must stay readable without the encryption key,
	// because it is the audit artefact.
	var termsURL string
	if err := b.store.db.QueryRow(`SELECT terms_url FROM accounts WHERE caller = ?`, "caller-alpha").Scan(&termsURL); err != nil {
		t.Fatalf("read terms: %v", err)
	}
	if termsURL != DefaultTermsURL {
		t.Errorf("terms url = %q, want it readable in the clear", termsURL)
	}
}

// New must refuse to start without the two things it cannot work without.
func TestNewRequiresPartnerKeyAndDomain(t *testing.T) {
	if _, err := New(Config{MailDomain: "x.example"}); err == nil {
		t.Error("expected an error with no partner key")
	}
	if _, err := New(Config{PartnerKey: "k"}); err == nil {
		t.Error("expected an error with no mail domain")
	}
}
