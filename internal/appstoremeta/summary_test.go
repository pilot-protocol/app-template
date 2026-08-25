// SPDX-License-Identifier: AGPL-3.0-or-later

package appstoremeta

import (
	"strings"
	"testing"
)

func TestFlattenStripsMarkdown(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		markdown string
		want     string
	}{
		{"plain text is unchanged", "Just a sentence.", "Just a sentence."},
		{"leading title is dropped", "# PostgreSQL — native CLI\n\nThis app installs it.", "This app installs it."},
		{"only the leading title is dropped", "# Title\n\nBody.\n\n## Section\n\nMore.", "Body. Section More."},
		{"bold markers go", "It is **really** fast.", "It is really fast."},
		{"italic markers go", "It is *really* fast.", "It is really fast."},
		{"code ticks go", "Call `deadsimple.signup` first.", "Call deadsimple.signup first."},
		{"link text survives, target does not", "See [Primitive](https://primitive.dev) now.", "See Primitive now."},
		{"fenced code blocks are removed", "Before.\n\n```sh\npilotctl call\n```\n\nAfter.", "Before. After."},
		{"whitespace collapses", "One\n\n\nTwo   Three\n", "One Two Three"},
		{"bullets keep their text", "Uses:\n\n- One thing\n- Another thing", "Uses: - One thing - Another thing"},
		{"empty stays empty", "", ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := Flatten(testCase.markdown); got != testCase.want {
				t.Errorf("Flatten(%q)\n got %q\nwant %q", testCase.markdown, got, testCase.want)
			}
		})
	}
}

func TestSummarizeKeepsShortCopyWhole(t *testing.T) {
	short := "A small app that does one thing well."
	if got := Summarize(short, 420); got != short {
		t.Errorf("short copy was altered: got %q", got)
	}
	if strings.HasSuffix(Summarize(short, 420), "…") {
		t.Error("short copy must not gain an ellipsis")
	}
}

func TestSummarizeTruncatesOnAWordBoundary(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("alpha bravo charlie delta ", 40))
	got := Summarize(long, 100)

	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated copy must end in an ellipsis: %q", got)
	}
	if len([]rune(got)) > 101 {
		t.Errorf("summary ran past the limit: %d runes", len([]rune(got)))
	}
	// A cut mid-word reads as a typo rather than a truncation.
	body := strings.TrimSuffix(got, "…")
	if strings.HasSuffix(body, " ") {
		t.Errorf("trailing space before the ellipsis: %q", got)
	}
	if !strings.HasPrefix(long, body) {
		t.Errorf("summary is not a prefix of the source: %q", body)
	}
	if last := body[strings.LastIndex(body, " ")+1:]; !strings.Contains(long, last+" ") {
		t.Errorf("cut mid-word on %q", last)
	}
}

func TestSummarizeCountsRunesNotBytes(t *testing.T) {
	// Em dashes and accents are three bytes and one glyph. Counting bytes cut
	// this copy roughly a third early, and could split a rune in half.
	long := strings.TrimSpace(strings.Repeat("café — naïve — ", 60))
	got := Summarize(long, 50)
	if runes := len([]rune(got)); runes > 51 || runes < 40 {
		t.Errorf("expected a rune-counted cut near 50, got %d runes: %q", runes, got)
	}
	if strings.Contains(got, "�") {
		t.Errorf("truncation split a rune: %q", got)
	}
}

func TestSummarizeFlattensFirst(t *testing.T) {
	got := Summarize("# Heading\n\nThe **body** copy.", 420)
	if got != "The body copy." {
		t.Errorf("got %q", got)
	}
}

func TestSummarizeZeroLimitMeansNoLimit(t *testing.T) {
	long := strings.Repeat("word ", 500)
	if got := Summarize(long, 0); strings.HasSuffix(got, "…") {
		t.Error("a zero limit must not truncate")
	}
}
