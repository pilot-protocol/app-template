package x402

import (
	"context"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// the real shape humwork.ai returns (Base / USDC / 10 USDC = 10_000_000 atoms).
const humworkChallenge = `{"x402Version":1,"accepts":[{"scheme":"exact","network":"eip155:8453","networkName":"base","asset":"USDC","payTo":"0xf97754eb7a82cde7a01e14f84a984299fc3bdad9","maxAmountRequired":"10000000","description":"Humwork expert consultation (10 min chunk)","resource":"https://api.humwork.ai/api/v1/tools/consult_expert","mimeType":"application/json"}]}`

func usdc(n int64) *big.Int { return big.NewInt(n) }

// TestSelect_EmptyAssetFailsClosed is a regression guard: an option that omits
// the asset must NOT satisfy an asset-capped config — its price can't be trusted
// to be denominated in the capped asset, so honoring it would defeat MaxAtomic.
func TestSelect_EmptyAssetFailsClosed(t *testing.T) {
	ch := &Challenge{Version: 1, Accepts: []Accepts{
		{NetworkName: "base", Asset: "", PayTo: "0xabc", MaxAmountRequired: "1"},
	}}

	// Asset configured → the empty-asset option is ineligible (fail closed).
	capped := Config{Networks: []string{"base"}, Asset: "USDC", MaxAtomic: usdc(10_000_000)}
	if _, err := capped.Select(ch); !errors.Is(err, ErrNoEligible) {
		t.Fatalf("empty-asset option must be ineligible under an asset cap, got %v", err)
	}

	// No asset configured → asset filter is off, so the option is selectable.
	anyAsset := Config{Networks: []string{"base"}, MaxAtomic: usdc(10_000_000)}
	if opt, err := anyAsset.Select(ch); err != nil || opt == nil {
		t.Fatalf("with no asset configured the option should select, got %v", err)
	}
}

type mockPayer struct {
	calls int32
	got   Accepts
	token string
	err   error
}

func (m *mockPayer) Satisfy(_ context.Context, a Accepts) (string, error) {
	atomic.AddInt32(&m.calls, 1)
	m.got = a
	return m.token, m.err
}

func TestParse(t *testing.T) {
	ch, err := Parse([]byte(humworkChallenge))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(ch.Accepts) != 1 || ch.Accepts[0].Network != "eip155:8453" || ch.Accepts[0].MaxAmountRequired != "10000000" {
		t.Fatalf("unexpected parse: %+v", ch.Accepts)
	}
	for _, bad := range []string{``, `not json`, `{"x402Version":1}`, `{"x402Version":1,"accepts":[]}`} {
		if _, err := Parse([]byte(bad)); !errors.Is(err, ErrNoChallenge) {
			t.Fatalf("Parse(%q): want ErrNoChallenge, got %v", bad, err)
		}
	}
}

func TestSelect(t *testing.T) {
	ch, _ := Parse([]byte(humworkChallenge))
	cases := []struct {
		name string
		cfg  Config
		want error
	}{
		{"base allowed under cap", Config{Networks: []string{"base"}, Asset: "USDC", MaxAtomic: usdc(10_000_000)}, nil},
		{"caip2 form allowed", Config{Networks: []string{"eip155:8453"}, MaxAtomic: usdc(20_000_000)}, nil},
		{"no network restriction", Config{MaxAtomic: usdc(10_000_000)}, nil},
		{"no cap at all", Config{Networks: []string{"base"}}, nil},
		{"network not allowed", Config{Networks: []string{"polygon"}, MaxAtomic: usdc(10_000_000)}, ErrNoEligible},
		{"asset mismatch", Config{Networks: []string{"base"}, Asset: "DAI", MaxAtomic: usdc(10_000_000)}, ErrNoEligible},
		{"over cap", Config{Networks: []string{"base"}, MaxAtomic: usdc(9_999_999)}, ErrOverCap},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opt, err := tc.cfg.Select(ch)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
				if opt.PayTo == "" {
					t.Fatal("selected option missing payTo")
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

// backend402 serves 402+challenge until a request carries X-PAYMENT, then 200.
func backend402(t *testing.T, wantBody string) (*httptest.Server, *int32, *int32) {
	t.Helper()
	var paid, total int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&total, 1)
		if b, _ := io.ReadAll(r.Body); wantBody != "" && string(b) != wantBody {
			t.Errorf("backend got body %q, want %q", string(b), wantBody)
		}
		if r.Header.Get(PaymentHeader) == "" {
			w.Header().Set("WWW-Authenticate", "Payment")
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = w.Write([]byte(humworkChallenge))
			return
		}
		atomic.AddInt32(&paid, 1)
		w.Header().Set(PaymentResponseHeader, "settled")
		_, _ = w.Write([]byte(`{"session_id":"sess_123"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &paid, &total
}

func newReq(t *testing.T, url, body string) *http.Request {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestClient_PaysAndRetries(t *testing.T) {
	srv, paid, total := backend402(t, `{"domain":"software"}`)
	payer := &mockPayer{token: "PAYMENT_AUTH_BLOB"}
	c := &Client{HTTP: srv.Client(), Payer: payer, Cfg: Config{Networks: []string{"base"}, Asset: "USDC", MaxAtomic: usdc(10_000_000)}}

	resp, err := c.Do(newReq(t, srv.URL, `{"domain":"software"}`))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("final status = %d, want 200", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&payer.calls); got != 1 {
		t.Fatalf("payer called %d times, want 1", got)
	}
	if payer.got.PayTo != "0xf97754eb7a82cde7a01e14f84a984299fc3bdad9" {
		t.Fatalf("payer got wrong option: %+v", payer.got)
	}
	if atomic.LoadInt32(total) != 2 || atomic.LoadInt32(paid) != 1 {
		t.Fatalf("backend hits: total=%d paid=%d, want 2/1", *total, *paid)
	}
	if b, _ := io.ReadAll(resp.Body); !strings.Contains(string(b), "sess_123") {
		t.Fatalf("unexpected final body: %s", b)
	}
}

func TestClient_NoPaymentWhen200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	payer := &mockPayer{token: "x"}
	c := &Client{HTTP: srv.Client(), Payer: payer, Cfg: Config{Networks: []string{"base"}}}
	resp, err := c.Do(newReq(t, srv.URL, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if atomic.LoadInt32(&payer.calls) != 0 {
		t.Fatal("payer should not be called on a 200")
	}
}

func TestClient_OverCapDoesNotPay(t *testing.T) {
	srv, _, _ := backend402(t, "")
	payer := &mockPayer{token: "x"}
	c := &Client{HTTP: srv.Client(), Payer: payer, Cfg: Config{Networks: []string{"base"}, MaxAtomic: usdc(1_000_000)}}
	_, err := c.Do(newReq(t, srv.URL, `{}`))
	if !errors.Is(err, ErrOverCap) {
		t.Fatalf("want ErrOverCap, got %v", err)
	}
	if atomic.LoadInt32(&payer.calls) != 0 {
		t.Fatal("payer must not be called when over cap")
	}
}

func TestClient_PayerErrorSurfaces(t *testing.T) {
	srv, _, _ := backend402(t, "")
	payer := &mockPayer{err: errors.New("insufficient balance")}
	c := &Client{HTTP: srv.Client(), Payer: payer, Cfg: Config{Networks: []string{"base"}, MaxAtomic: usdc(10_000_000)}}
	_, err := c.Do(newReq(t, srv.URL, `{}`))
	if err == nil || !strings.Contains(err.Error(), "insufficient balance") {
		t.Fatalf("want wallet error surfaced, got %v", err)
	}
}

// If the backend still 402s after payment, return it — never loop.
func TestClient_SecondChallengeReturnedNotLooped(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(humworkChallenge))
	}))
	defer srv.Close()
	payer := &mockPayer{token: "x"}
	c := &Client{HTTP: srv.Client(), Payer: payer, Cfg: Config{Networks: []string{"base"}, MaxAtomic: usdc(10_000_000)}}
	resp, err := c.Do(newReq(t, srv.URL, `{}`))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("want 402 returned, got %d", resp.StatusCode)
	}
	if atomic.LoadInt32(&hits) != 2 || atomic.LoadInt32(&payer.calls) != 1 {
		t.Fatalf("want exactly 2 backend hits + 1 pay, got hits=%d pay=%d", hits, payer.calls)
	}
}
