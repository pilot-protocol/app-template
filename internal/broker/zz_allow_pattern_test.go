package broker

import "testing"

// Templated allow entries ("{name}" segments) let REST path params through
// without enumerating every id, while still refusing un-allowed paths.
func TestAllowPatternMatching(t *testing.T) {
	reg, err := ParseRegistry([]byte(`[{
		"id":"io.pilot.x","upstream":"https://api.example.com","key_env":"X_KEY",
		"auth_header":"Authorization","auth_scheme":"Bearer",
		"allow":["/v1/usage","/v1/calls/{call_id}","/v1/numbers/{number_id}/messages"]
	}]`), func(string) string { return "secret" })
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	app := reg.Get("io.pilot.x")
	if app == nil {
		t.Fatal("app not loaded")
	}
	cases := []struct {
		path string
		want bool
	}{
		{"/v1/usage", true},                  // exact
		{"/v1/calls/call_abc123", true},      // one path param
		{"/v1/calls/", false},                // empty segment must not match {call_id}
		{"/v1/calls/abc/extra", false},       // too many segments
		{"/v1/numbers/num_1/messages", true}, // param in the middle
		{"/v1/numbers/num_1/calls", false},   // literal tail mismatch
		{"/v1/agents", false},                // not allowed at all
		{"/v1/usage/daily", false},           // longer than the exact entry
	}
	for _, c := range cases {
		if got := app.allowed(c.path); got != c.want {
			t.Errorf("allowed(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// An empty allow-list permits nothing (safe default), even with no patterns.
func TestAllowEmptyDeniesAll(t *testing.T) {
	reg, err := ParseRegistry([]byte(`[{"id":"io.pilot.y","upstream":"https://api.example.com","key_env":"Y_KEY","allow":[]}]`),
		func(string) string { return "secret" })
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	if reg.Get("io.pilot.y").allowed("/anything") {
		t.Error("empty allow-list must deny all")
	}
}
