package main

import "testing"

func TestBumpSemver(t *testing.T) {
	cases := []struct{ v, part, want string }{
		{"0.1.0", "patch", "0.1.1"},
		{"0.1.0", "minor", "0.2.0"},
		{"0.1.0", "major", "1.0.0"},
		{"1.2.3", "minor", "1.3.0"},
		{"1.2.3-rc.1", "patch", "1.2.4"}, // prerelease dropped
	}
	for _, c := range cases {
		got, err := bumpSemver(c.v, c.part)
		if err != nil {
			t.Fatalf("bump(%s,%s): %v", c.v, c.part, err)
		}
		if got != c.want {
			t.Errorf("bump(%s,%s) = %s, want %s", c.v, c.part, got, c.want)
		}
	}
	if _, err := bumpSemver("1.2", "patch"); err == nil {
		t.Error("expected error for non-3-part version")
	}
	if _, err := bumpSemver("1.2.3", "nope"); err == nil {
		t.Error("expected error for bad part")
	}
}

// The version is rewritten in place, preserving indentation, quoting, and the
// trailing comment — and nothing else in the document is touched.
func TestRewriteAppVersion(t *testing.T) {
	in := []byte("id: io.pilot.x\napp_version: 0.1.0   # bump me\ndescription: y\n")
	out, ok := rewriteAppVersion(in, "0.2.0")
	if !ok {
		t.Fatal("expected a match")
	}
	want := "id: io.pilot.x\napp_version: 0.2.0   # bump me\ndescription: y\n"
	if string(out) != want {
		t.Fatalf("got %q, want %q", out, want)
	}

	// quoted value
	q, ok := rewriteAppVersion([]byte(`app_version: "1.0.0"`+"\n"), "1.0.1")
	if !ok || string(q) != `app_version: "1.0.1"`+"\n" {
		t.Fatalf("quoted rewrite failed: %q", q)
	}

	if _, ok := rewriteAppVersion([]byte("id: io.pilot.x\n"), "0.2.0"); ok {
		t.Error("expected no match when app_version is absent")
	}
}
