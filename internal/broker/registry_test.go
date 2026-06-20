package broker

import (
	"path/filepath"
	"testing"
)

func TestLoadRegistry_MissingFileIsEmpty(t *testing.T) {
	reg, err := LoadRegistry(filepath.Join(t.TempDir(), "nope.json"), func(string) string { return "K" })
	if err != nil {
		t.Fatalf("missing registry should not error: %v", err)
	}
	if reg.Get("anything") != nil {
		t.Fatal("empty registry should know no apps")
	}
}

func TestParseRegistry_EmptyIsEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\n"} {
		reg, err := ParseRegistry([]byte(in), func(string) string { return "K" })
		if err != nil {
			t.Fatalf("empty registry %q should not error: %v", in, err)
		}
		if reg.Get("x") != nil {
			t.Fatal("expected no apps")
		}
	}
}

func TestParseRegistry_MalformedErrors(t *testing.T) {
	if _, err := ParseRegistry([]byte(`{not json`), func(string) string { return "K" }); err == nil {
		t.Fatal("malformed registry must error (not be silently treated as empty)")
	}
}

func TestParseRegistry_MissingKeyEnvErrors(t *testing.T) {
	raw := []byte(`[{"id":"io.pilot.x","upstream":"https://api.x","key_env":"X_KEY","allow":["/go"]}]`)
	if _, err := ParseRegistry(raw, func(string) string { return "" }); err == nil {
		t.Fatal("an app whose master key env is unset must fail to load")
	}
}
