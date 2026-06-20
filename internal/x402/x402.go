// Package x402 adds transparent, capped payment to an HTTP backend client.
//
// When a backend answers a request with HTTP 402 and an x402 challenge body,
// the adapter parses the payment options, picks one that fits the operator's
// allow-list and per-call cap, asks a Payer (the Pilot wallet, reached over the
// daemon IPC broker) to satisfy it, and retries the request once with the
// resulting X-PAYMENT header. The agent never sees the 402.
//
// This package is the negotiation core: pure, side-effect-free except for the
// injected Payer and HTTP Doer, so it is fully unit-testable without a daemon,
// a wallet, or a chain. The real Payer lives in the generated adapter and calls
// io.pilot.wallet.evm.satisfy; tests inject a mock.
package x402

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
)

// PaymentHeader is the request header the client sets on the retried request.
// PaymentResponseHeader is the receipt the server returns on success (x402).
const (
	PaymentHeader         = "X-PAYMENT"
	PaymentResponseHeader = "X-PAYMENT-RESPONSE"
)

var (
	// ErrNoChallenge means the 402 body wasn't a parseable x402 challenge.
	ErrNoChallenge = errors.New("x402: response is 402 but carries no usable payment challenge")
	// ErrNoEligible means no option matched the network/asset allow-list.
	ErrNoEligible = errors.New("x402: no payment option matches the configured networks/asset")
	// ErrOverCap means a matching option's price exceeds the per-call cap.
	ErrOverCap = errors.New("x402: payment exceeds the per-call cap")
)

// Accepts is one payment option from an x402 challenge body.
type Accepts struct {
	Scheme            string `json:"scheme"`
	Network           string `json:"network"`     // CAIP-2, e.g. "eip155:8453"
	NetworkName       string `json:"networkName"` // friendly, e.g. "base"
	Asset             string `json:"asset"`       // e.g. "USDC"
	PayTo             string `json:"payTo"`
	MaxAmountRequired string `json:"maxAmountRequired"` // atomic units, decimal string
	Description       string `json:"description"`
	Resource          string `json:"resource"`
	MimeType          string `json:"mimeType"`
}

// Challenge is the parsed x402 402 body.
type Challenge struct {
	Version int       `json:"x402Version"`
	Accepts []Accepts `json:"accepts"`
}

// Payer satisfies one x402 option, returning the opaque X-PAYMENT header value
// (a signed payment authorization). The real implementation calls the Pilot
// wallet over the daemon IPC broker.
type Payer interface {
	Satisfy(ctx context.Context, a Accepts) (xPayment string, err error)
}

// Config controls which charges the adapter will auto-pay. It's a guard rail in
// front of the wallet's own spend caps, not a replacement for them.
type Config struct {
	Networks  []string // allowed networks (CAIP-2 or friendly name), case-insensitive; empty = any
	Asset     string   // required asset symbol, e.g. "USDC"; empty = any
	MaxAtomic *big.Int // per-call cap in atomic units; nil = rely on the wallet's caps only
}

func (c Config) networkOK(a Accepts) bool {
	if len(c.Networks) == 0 {
		return true
	}
	for _, n := range c.Networks {
		if strings.EqualFold(n, a.Network) || strings.EqualFold(n, a.NetworkName) {
			return true
		}
	}
	return false
}

// Select picks the first option that matches the network/asset allow-list and
// fits the cap. It distinguishes "nothing matched the allow-list" from "matched
// but too expensive" so the agent gets an actionable error.
func (c Config) Select(ch *Challenge) (*Accepts, error) {
	if ch == nil || len(ch.Accepts) == 0 {
		return nil, ErrNoChallenge
	}
	var sawEligible bool
	var available []string
	for i := range ch.Accepts {
		a := ch.Accepts[i]
		net := a.NetworkName
		if net == "" {
			net = a.Network
		}
		available = append(available, net)
		if !c.networkOK(a) {
			continue
		}
		if c.Asset != "" && a.Asset != "" && !strings.EqualFold(c.Asset, a.Asset) {
			continue
		}
		sawEligible = true
		if c.MaxAtomic != nil {
			amt, ok := new(big.Int).SetString(strings.TrimSpace(a.MaxAmountRequired), 10)
			if !ok {
				continue // unparseable price; skip rather than overpay
			}
			if amt.Cmp(c.MaxAtomic) > 0 {
				continue
			}
		}
		return &a, nil
	}
	if sawEligible {
		return nil, fmt.Errorf("%w (cap %s atomic units)", ErrOverCap, capStr(c.MaxAtomic))
	}
	return nil, fmt.Errorf("%w: offered %s, allowed %v", ErrNoEligible, strings.Join(available, ", "), c.Networks)
}

func capStr(n *big.Int) string {
	if n == nil {
		return "none"
	}
	return n.String()
}

// Parse reads an x402 challenge from a 402 response body.
func Parse(body []byte) (*Challenge, error) {
	var ch Challenge
	if err := json.Unmarshal(body, &ch); err != nil {
		return nil, ErrNoChallenge
	}
	if len(ch.Accepts) == 0 {
		return nil, ErrNoChallenge
	}
	return &ch, nil
}

// Doer is the minimal HTTP surface (satisfied by *http.Client).
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client wraps a Doer: a 402 triggers one capped pay-and-retry.
type Client struct {
	HTTP  Doer
	Cfg   Config
	Payer Payer
}

// Do sends req. On a 402 with a usable, in-policy x402 challenge it asks the
// Payer to satisfy it and retries the identical request once with X-PAYMENT.
// Any other status (including a second 402) is returned as-is.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	// Buffer the body so the request can be replayed on the paid retry.
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("x402: buffering request body: %w", err)
		}
		body = b
		req.Body = io.NopCloser(bytes.NewReader(body))
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusPaymentRequired || c.Payer == nil {
		return resp, nil
	}

	// Read + close the 402 body before retrying.
	chBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	ch, perr := Parse(chBody)
	if perr != nil {
		return nil, perr
	}
	opt, serr := c.Cfg.Select(ch)
	if serr != nil {
		return nil, serr
	}
	xpay, perr := c.Payer.Satisfy(req.Context(), *opt)
	if perr != nil {
		return nil, fmt.Errorf("x402: wallet could not satisfy charge: %w", perr)
	}

	retry := req.Clone(req.Context())
	if body != nil {
		retry.Body = io.NopCloser(bytes.NewReader(body))
		retry.ContentLength = int64(len(body))
	}
	retry.Header.Set(PaymentHeader, xpay)
	return c.HTTP.Do(retry)
}
