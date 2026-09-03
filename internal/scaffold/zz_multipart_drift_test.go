package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The blob store and form builder emitted into every multipart app are copies
// of internal/multipartkit, which is where they are unit-tested. Two copies of
// security-relevant code is a drift hazard — the emitted one is what actually
// runs on a user's host, and a fix applied only to the tested copy would be a
// fix that never ships.
//
// This is the same stance canonical_golden_test.go takes for the signer, whose
// bytes must match the broker's verifier. The templates are derived from the
// reference by changing the package clause and nothing else, so the check is an
// exact comparison rather than a spot-check of a few constants.
var driftPairs = []struct{ ref, tmpl string }{
	{filepath.Join("..", "multipartkit", "blob.go"), filepath.Join("templates", "blob.go.tmpl")},
	{filepath.Join("..", "multipartkit", "form.go"), filepath.Join("templates", "multipartform.go.tmpl")},
}

func TestEmittedMultipartCodeMatchesReference(t *testing.T) {
	for _, p := range driftPairs {
		ref, err := os.ReadFile(p.ref)
		if err != nil {
			t.Fatalf("read reference %s: %v", p.ref, err)
		}
		got, err := os.ReadFile(p.tmpl)
		if err != nil {
			t.Fatalf("read template %s: %v", p.tmpl, err)
		}
		want := strings.Replace(string(ref), "package multipartkit", "package backend", 1)
		if string(got) != want {
			t.Errorf("%s has drifted from %s.\n"+
				"The emitted copy is what runs on a user's host, so a change to one must be made to both.\n"+
				"Regenerate with:\n"+
				"  sed 's/^package multipartkit$/package backend/' %s > internal/scaffold/%s",
				p.tmpl, p.ref, p.ref, p.tmpl)
		}
	}
}

// TestEmittedMultipartTemplatesHaveNoTemplateDelimiters: these two files are
// rendered through text/template like every other template, so a `{{` appearing
// in the reference source would be interpreted as an action and either fail the
// render or silently delete code. Nothing in Go source needs `{{`, but a future
// edit could introduce one (a nested composite literal written without a space),
// and the failure would be confusing far from its cause.
func TestEmittedMultipartTemplatesHaveNoTemplateDelimiters(t *testing.T) {
	for _, p := range driftPairs {
		b, err := os.ReadFile(p.tmpl)
		if err != nil {
			t.Fatalf("read %s: %v", p.tmpl, err)
		}
		if i := strings.Index(string(b), "{{"); i >= 0 {
			line := 1 + strings.Count(string(b[:i]), "\n")
			t.Errorf("%s line %d contains `{{`, which text/template will interpret as an action; separate the braces", p.tmpl, line)
		}
	}
}
