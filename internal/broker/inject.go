package broker

import (
	"encoding/base64"
	"net/http"
)

// AuthInjector puts the app's master credential on the outbound request to the
// partner API. It is the auth seam: different partners authenticate differently
// (header, bearer, query param, basic), so the strategy is per-app and behind
// this interface. The caller's headers are never forwarded — only what an
// injector sets reaches the partner.
type AuthInjector interface {
	Inject(req *http.Request, master string)
}

// HeaderInjector sets a header to (scheme + " " + master), or just master when
// scheme is empty. Covers "x-api-key: <key>" and "Authorization: Bearer <key>".
type HeaderInjector struct {
	Name   string
	Scheme string
}

func (h HeaderInjector) Inject(req *http.Request, master string) {
	v := master
	if h.Scheme != "" {
		v = h.Scheme + " " + master
	}
	req.Header.Set(h.Name, v)
}

// QueryInjector adds the master key as a query parameter (e.g. ?apikey=<key>).
type QueryInjector struct{ Param string }

func (q QueryInjector) Inject(req *http.Request, master string) {
	vals := req.URL.Query()
	vals.Set(q.Param, master)
	req.URL.RawQuery = vals.Encode()
}

// BasicInjector sends HTTP Basic auth with the master key as the username and an
// empty password (the common "API key as username" pattern, e.g. Stripe).
type BasicInjector struct{ User string }

func (b BasicInjector) Inject(req *http.Request, master string) {
	user := b.User
	pass := master
	if user == "" { // default: key-as-username, empty password
		user = master
		pass = ""
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
}

// injectorFor builds the AuthInjector for a registry entry from its auth_style.
// "" / "header" → HeaderInjector (default), "query" → QueryInjector,
// "basic" → BasicInjector. Scheme/header/param come from the entry fields.
func injectorFor(style, header, scheme, param, user string) AuthInjector {
	switch style {
	case "query":
		return QueryInjector{Param: param}
	case "basic":
		return BasicInjector{User: user}
	default:
		return HeaderInjector{Name: header, Scheme: scheme}
	}
}
