package broker

import (
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHeaderInjector(t *testing.T) {
	req := httptest.NewRequest("POST", "http://x/y", nil)
	HeaderInjector{Name: "x-api-key"}.Inject(req, "SECRET")
	if got := req.Header.Get("x-api-key"); got != "SECRET" {
		t.Fatalf("x-api-key = %q, want SECRET", got)
	}

	req2 := httptest.NewRequest("POST", "http://x/y", nil)
	HeaderInjector{Name: "Authorization", Scheme: "Bearer"}.Inject(req2, "SECRET")
	if got := req2.Header.Get("Authorization"); got != "Bearer SECRET" {
		t.Fatalf("Authorization = %q, want 'Bearer SECRET'", got)
	}
}

func TestQueryInjector(t *testing.T) {
	req := httptest.NewRequest("POST", "http://x/y?q=1", nil)
	QueryInjector{Param: "apikey"}.Inject(req, "SECRET")
	if got := req.URL.Query().Get("apikey"); got != "SECRET" {
		t.Fatalf("apikey = %q, want SECRET", got)
	}
	if got := req.URL.Query().Get("q"); got != "1" {
		t.Fatalf("existing query param dropped: q = %q", got)
	}
}

func TestBasicInjector_KeyAsUsername(t *testing.T) {
	req := httptest.NewRequest("POST", "http://x/y", nil)
	BasicInjector{}.Inject(req, "SECRET")
	got := req.Header.Get("Authorization")
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("SECRET:"))
	if got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

func TestBasicInjector_ExplicitUser(t *testing.T) {
	req := httptest.NewRequest("POST", "http://x/y", nil)
	BasicInjector{User: "u"}.Inject(req, "SECRET")
	got := req.Header.Get("Authorization")
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("u:SECRET"))
	if got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

func TestInjectorFor_SelectsByStyle(t *testing.T) {
	cases := []struct {
		style  string
		assert func(AuthInjector) bool
	}{
		{"", func(i AuthInjector) bool { _, ok := i.(HeaderInjector); return ok }},
		{"header", func(i AuthInjector) bool { _, ok := i.(HeaderInjector); return ok }},
		{"query", func(i AuthInjector) bool { _, ok := i.(QueryInjector); return ok }},
		{"basic", func(i AuthInjector) bool { _, ok := i.(BasicInjector); return ok }},
	}
	for _, c := range cases {
		got := injectorFor(c.style, "x-api-key", "", "apikey", "")
		if !c.assert(got) {
			t.Fatalf("style %q: wrong injector type %T", c.style, got)
		}
	}
}

// ParseRegistry should build the right injector from auth_style.
func TestRegistry_QueryStyleInjector(t *testing.T) {
	raw := []byte(`[{"id":"io.pilot.q","upstream":"https://api.x","key_env":"K","auth_style":"query","auth_param":"apikey","allow":["/go"],"quota":1}]`)
	reg, err := ParseRegistry(raw, func(string) string { return "SECRET" })
	if err != nil {
		t.Fatal(err)
	}
	app := reg.Get("io.pilot.q")
	if _, ok := app.injector.(QueryInjector); !ok {
		t.Fatalf("expected QueryInjector, got %T", app.injector)
	}
	req := httptest.NewRequest("POST", app.Upstream+"/go", nil)
	app.injector.Inject(req, app.master)
	if !strings.Contains(req.URL.RawQuery, "apikey=SECRET") {
		t.Fatalf("query not injected: %s", req.URL.RawQuery)
	}
}
