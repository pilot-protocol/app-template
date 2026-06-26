package scaffold

import "testing"

// paramInYAML builds a single-method http spec with explicit param + the
// per-param `param_in` map (nested under http, where HTTPRoute.ParamIn lives).
func paramInYAML(path, params, paramIn string) string {
	http := "{ verb: GET, path: \"" + path + "\""
	if paramIn != "" {
		http += ", param_in: { " + paramIn + " }"
	}
	http += " }"
	m := "  - name: x.m\n    http: " + http + "\n"
	if params != "" {
		m += "    params: { " + params + " }\n"
	}
	return baseYAML + m
}

// Resolve sorts each explicit `in` param into the right bucket; a {name}
// placeholder defaults to escaped path, and path_raw upgrades it to the raw
// bucket. Params with no `in` are left out of the explicit buckets (residual).
func TestParamInResolvesBuckets(t *testing.T) {
	c := parseResolved(t, paramInYAML(
		"/{url}",
		"url: u, q: query term, key: api key, note: a note, extra: residual",
		"url: path_raw, q: query, key: header, note: body",
	))
	h := c.Methods[0].HTTP
	if len(h.RawPathParams) != 1 || h.RawPathParams[0] != "url" {
		t.Errorf("RawPathParams = %v, want [url]", h.RawPathParams)
	}
	if len(h.PathParams) != 0 {
		t.Errorf("PathParams = %v, want [] (url is path_raw)", h.PathParams)
	}
	if len(h.QueryParams) != 1 || h.QueryParams[0] != "q" {
		t.Errorf("QueryParams = %v, want [q]", h.QueryParams)
	}
	if len(h.HeaderParams) != 1 || h.HeaderParams[0] != "key" {
		t.Errorf("HeaderParams = %v, want [key]", h.HeaderParams)
	}
	if len(h.BodyParams) != 1 || h.BodyParams[0] != "note" {
		t.Errorf("BodyParams = %v, want [note]", h.BodyParams)
	}
}

// A {name} placeholder with no explicit `in` stays an escaped path param —
// the historical default, so existing specs are unchanged.
func TestPlaceholderDefaultsToEscapedPath(t *testing.T) {
	h := parseResolved(t, paramInYAML("/v1/things/{id}", "id: the id", "")).Methods[0].HTTP
	if len(h.PathParams) != 1 || h.PathParams[0] != "id" || len(h.RawPathParams) != 0 {
		t.Errorf("default placeholder: PathParams=%v RawPathParams=%v, want escaped [id]", h.PathParams, h.RawPathParams)
	}
}

// path / path_raw require a matching {name} placeholder; an unknown `in` value
// and an `in` on an undeclared param are rejected — clear, easy-to-configure.
func TestParamInValidation(t *testing.T) {
	// path with no placeholder.
	c := parseResolved(t, paramInYAML("/search", "u: a url", "u: path_raw"))
	if !hasErrContaining(c.Validate(), "has no {u} placeholder") {
		t.Errorf("expected missing-placeholder error, got %v", c.Validate())
	}
	// invalid `in` value.
	c = parseResolved(t, paramInYAML("/search", "q: a query", "q: cookie"))
	if !hasErrContaining(c.Validate(), "must be query|path|path_raw|body|header") {
		t.Errorf("expected invalid-in error, got %v", c.Validate())
	}
	// `in` on an undeclared param.
	c = parseResolved(t, paramInYAML("/search", "q: a query", "ghost: query"))
	if !hasErrContaining(c.Validate(), "is not declared under params") {
		t.Errorf("expected undeclared-param error, got %v", c.Validate())
	}
	// fully valid: path_raw placeholder + query + header.
	c = parseResolved(t, paramInYAML("/{url}", "url: u, q: query, key: api key", "url: path_raw, q: query, key: header"))
	if errs := c.Validate(); len(errs) != 0 {
		t.Errorf("valid param_in rejected: %v", errs)
	}
}
