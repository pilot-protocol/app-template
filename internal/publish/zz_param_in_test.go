package publish

import (
	"strings"
	"testing"
)

// A path_raw URL-in-path method (the plainweb shape) validates and carries the
// param location through ToConfig into the buildable scaffold route.
func TestSubmissionParamInPathRaw(t *testing.T) {
	s := sampleSubmission()
	s.Backend.BaseURL = "https://plainweb-hu4fob755a-uc.a.run.app"
	s.Methods = []SubMethod{{
		Name: "weather.fetch", Description: "Fetch a URL.", Latency: "med",
		HTTP:   SubRoute{Verb: "GET", Path: "/{url}"},
		Params: []SubParam{{Name: "url", Type: "string", Required: true, In: "path_raw"}},
	}}
	if errs := s.Validate(); len(errs) != 0 {
		t.Fatalf("valid path_raw submission rejected: %v", errs)
	}
	cfg := s.ToConfig()
	if errs := cfg.Validate(); len(errs) != 0 {
		t.Fatalf("derived config invalid: %v", errs)
	}
	h := cfg.Methods[0].HTTP
	if h.ParamIn["url"] != "path_raw" {
		t.Errorf("ParamIn not carried: %v", h.ParamIn)
	}
	if len(h.RawPathParams) != 1 || h.RawPathParams[0] != "url" {
		t.Errorf("url not resolved to a raw path param: %v", h.RawPathParams)
	}
	if len(h.PathParams) != 0 {
		t.Errorf("url should not be in escaped PathParams: %v", h.PathParams)
	}
}

// Each of the five locations carries through, and a mix on one method is allowed.
func TestSubmissionParamInMixAndAllLocations(t *testing.T) {
	s := sampleSubmission()
	s.Methods = []SubMethod{{
		Name: "weather.q", Description: "Mixed.", Latency: "fast",
		HTTP: SubRoute{Verb: "POST", Path: "/v1/{id}/go"},
		Params: []SubParam{
			{Name: "id", Type: "string", Required: true, In: "path"},
			{Name: "filter", Type: "string", In: "query"},
			{Name: "payload", Type: "string", In: "body"},
			{Name: "token", Type: "string", In: "header"},
		},
	}}
	if errs := s.Validate(); len(errs) != 0 {
		t.Fatalf("valid mixed submission rejected: %v", errs)
	}
	h := s.ToConfig().Methods[0].HTTP
	if len(h.PathParams) != 1 || len(h.QueryParams) != 1 || len(h.BodyParams) != 1 || len(h.HeaderParams) != 1 {
		t.Errorf("mixed buckets wrong: path=%v query=%v body=%v header=%v",
			h.PathParams, h.QueryParams, h.BodyParams, h.HeaderParams)
	}
}

// Bad `in` values and a path/path_raw param without a placeholder are rejected
// at the submission boundary with clear, server-authoritative errors.
func TestSubmissionParamInRejections(t *testing.T) {
	bad := sampleSubmission()
	bad.Methods = []SubMethod{{
		Name: "weather.x", Description: "d", Latency: "fast",
		HTTP:   SubRoute{Verb: "GET", Path: "/x"},
		Params: []SubParam{{Name: "u", Type: "string", In: "cookie"}},
	}}
	if !hasSub(bad.Validate(), "must be one of query, path, path_raw, body, header") {
		t.Errorf("expected invalid in-value error, got %v", bad.Validate())
	}

	noPlaceholder := sampleSubmission()
	noPlaceholder.Methods = []SubMethod{{
		Name: "weather.x", Description: "d", Latency: "fast",
		HTTP:   SubRoute{Verb: "GET", Path: "/search"},
		Params: []SubParam{{Name: "u", Type: "string", In: "path_raw"}},
	}}
	if !hasSub(noPlaceholder.Validate(), "needs a matching {u} placeholder") {
		t.Errorf("expected missing-placeholder error, got %v", noPlaceholder.Validate())
	}
}

// Omitting `in` everywhere keeps the historical defaults: the derived config is
// identical to a submission that predates the field (back-compat).
func TestSubmissionNoInIsBackCompat(t *testing.T) {
	s := sampleSubmission() // no `in` anywhere
	if errs := s.Validate(); len(errs) != 0 {
		t.Fatalf("plain submission rejected: %v", errs)
	}
	h := s.ToConfig().Methods[0].HTTP
	if len(h.ParamIn) != 0 {
		t.Errorf("ParamIn should be empty when no `in` is set, got %v", h.ParamIn)
	}
	if len(h.QueryParams) != 0 || len(h.BodyParams) != 0 || len(h.HeaderParams) != 0 || len(h.RawPathParams) != 0 {
		t.Errorf("no-`in` method should have empty explicit buckets, got q=%v b=%v h=%v raw=%v",
			h.QueryParams, h.BodyParams, h.HeaderParams, h.RawPathParams)
	}
}

func hasSub(errs []string, sub string) bool {
	for _, e := range errs {
		if strings.Contains(e, sub) {
			return true
		}
	}
	return false
}
