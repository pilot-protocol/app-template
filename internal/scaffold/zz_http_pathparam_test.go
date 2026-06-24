package scaffold

import (
	"strings"
	"testing"
)

func parseResolved(t *testing.T, yaml string) *Config {
	t.Helper()
	c, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c.Resolve()
	return c
}

func hasErrContaining(errs []error, sub string) bool {
	for _, e := range errs {
		if strings.Contains(e.Error(), sub) {
			return true
		}
	}
	return false
}

const baseYAML = `id: io.pilot.x
app_version: 0.1.0
description: test app
backend:
  base_url: https://api.example.com
methods:
`

func methodYAML(verb, path, params string) string {
	// The path is quoted: a {placeholder} is a YAML flow-map indicator unquoted.
	m := "  - name: x.m\n    http: { verb: " + verb + ", path: \"" + path + "\" }\n"
	if params != "" {
		m += "    params: { " + params + " }\n"
	}
	return baseYAML + m
}

// All five REST verbs validate (and lower-case is normalised).
func TestHTTPVerbsAccepted(t *testing.T) {
	for _, v := range []string{"GET", "POST", "PATCH", "PUT", "DELETE", "get", "delete"} {
		c := parseResolved(t, methodYAML(v, "/v1/things", ""))
		if errs := c.Validate(); len(errs) != 0 {
			t.Errorf("verb %q: unexpected errors: %v", v, errs)
		}
	}
}

func TestHTTPVerbRejected(t *testing.T) {
	c := parseResolved(t, methodYAML("CONNECT", "/v1/things", ""))
	if !hasErrContaining(c.Validate(), "must be GET|POST|PATCH|PUT|DELETE") {
		t.Errorf("expected verb rejection, got %v", c.Validate())
	}
}

// A {placeholder} with no matching params: entry is a spec error.
func TestPathParamMustBeDeclared(t *testing.T) {
	c := parseResolved(t, methodYAML("GET", "/v1/calls/{call_id}", ""))
	if !hasErrContaining(c.Validate(), "path placeholder {call_id}") {
		t.Errorf("expected undeclared path-param error, got %v", c.Validate())
	}
	ok := parseResolved(t, methodYAML("GET", "/v1/calls/{call_id}", "call_id: the call id"))
	if errs := ok.Validate(); len(errs) != 0 {
		t.Errorf("declared path param: unexpected errors: %v", errs)
	}
}

// Resolve derives PathParams from the path, in order; BodyVerb classifies verbs.
func TestPathParamsDerivedAndBodyVerb(t *testing.T) {
	c := parseResolved(t, methodYAML("PATCH", "/v1/numbers/{number_id}/agents/{agent_id}", "number_id: a, agent_id: b"))
	h := c.Methods[0].HTTP
	if len(h.PathParams) != 2 || h.PathParams[0] != "number_id" || h.PathParams[1] != "agent_id" {
		t.Errorf("PathParams = %v, want [number_id agent_id]", h.PathParams)
	}
	if !h.BodyVerb() {
		t.Error("PATCH should be a body verb")
	}
	g := parseResolved(t, methodYAML("GET", "/v1/x", "")).Methods[0].HTTP
	if g.BodyVerb() {
		t.Error("GET should not be a body verb")
	}
	d := parseResolved(t, methodYAML("DELETE", "/v1/x/{id}", "id: i")).Methods[0].HTTP
	if d.BodyVerb() {
		t.Error("DELETE should not be a body verb")
	}
}
