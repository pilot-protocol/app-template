// SPDX-License-Identifier: AGPL-3.0-or-later

package appstoremeta

import (
	"regexp"
	"strings"
)

// SummaryLimit is how much flattened copy a card carries. It is the length the
// management console's hand-maintained snapshot had settled on, kept so the
// switch to a served document does not reflow every card in the store.
const SummaryLimit = 420

var (
	// A fenced block is code, not prose: dropping it whole beats flattening a
	// shell transcript into the middle of a sentence.
	fencedBlock  = regexp.MustCompile("(?s)```.*?```")
	leadingTitle = regexp.MustCompile(`\A\s*#{1,6}[^\n]*\n`)
	headingMark  = regexp.MustCompile(`(?m)^\s{0,3}#{1,6}\s*`)
	boldMark     = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	italicMark   = regexp.MustCompile(`(^|[^*])\*([^*\n]+)\*`)
	codeSpan     = regexp.MustCompile("`([^`]*)`")
	inlineLink   = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	bulletMark   = regexp.MustCompile(`(?m)^\s*[-*+]\s+`)
	blockQuote   = regexp.MustCompile(`(?m)^\s*>\s?`)
	whitespace   = regexp.MustCompile(`\s+`)
)

// Flatten renders authored Markdown as the single-paragraph plain text a card
// shows. It is deliberately not a Markdown parser: the copy it handles is
// prose with emphasis, code spans, links and bullets, and a real parser would
// be a dependency and a build step for no gain.
//
// The leading H1 is dropped because it repeats the app's name, which the card
// already renders directly above the summary.
func Flatten(markdown string) string {
	text := fencedBlock.ReplaceAllString(markdown, " ")
	text = leadingTitle.ReplaceAllString(text, "")
	text = headingMark.ReplaceAllString(text, "")
	text = blockQuote.ReplaceAllString(text, "")
	text = bulletMark.ReplaceAllString(text, "- ")
	text = inlineLink.ReplaceAllString(text, "$1")
	text = boldMark.ReplaceAllString(text, "$1")
	text = italicMark.ReplaceAllString(text, "$1$2")
	text = codeSpan.ReplaceAllString(text, "$1")
	return strings.TrimSpace(whitespace.ReplaceAllString(text, " "))
}

// Summarize flattens markdown and caps it at limit runes, cutting on a word
// boundary and marking the cut with an ellipsis. A limit of zero means no cap.
//
// The cap counts runes: this copy is full of em dashes and accented names, and
// a byte cap both truncated a third early and could halve a rune.
func Summarize(markdown string, limit int) string {
	text := Flatten(markdown)
	if limit <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}

	// One rune of the budget belongs to the ellipsis itself.
	cut := string(runes[:limit-1])
	if space := strings.LastIndexAny(cut, " \t\n"); space > 0 {
		cut = cut[:space]
	}
	// Trailing punctuation before an ellipsis reads as a typo.
	return strings.TrimRight(cut, " ,;:—-") + "…"
}
