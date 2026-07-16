package nextsteps

import (
	"fmt"
	"strings"
)

// RenderCall produces the block printed after a completed `pilotctl appstore
// call`. It is the whole point of the package, and its budget is brutal: this
// text lands after EVERY call, so anything longer than a few lines is noise an
// agent learns to skip — at which point the feature has made things worse than
// silence.
//
// That is why it is nothing like demo.RenderInstall's framed banner. Install
// happens once and can afford ceremony; this happens constantly and cannot.
// Shape:
//
//	next: budget exhausted — top up before retrying
//	  1. pilotctl appstore call io.pilot.wallet wallet.balance '{}'
//	     why: check remaining USDC before you spend again
//
// Output is deterministic — safe to golden-test.
func (e *Edge) RenderCall() string {
	if e == nil || len(e.Then) == 0 {
		return ""
	}
	var b strings.Builder
	if w := strings.TrimSpace(e.Why); w != "" {
		fmt.Fprintf(&b, "next: %s\n", oneLine(w))
	} else {
		b.WriteString("next:\n")
	}
	for i, s := range e.Then {
		if i >= maxThen {
			break
		}
		fmt.Fprintf(&b, "  %d. %s\n", i+1, strings.TrimSpace(s.Cmd))
		if w := strings.TrimSpace(s.Why); w != "" {
			fmt.Fprintf(&b, "     why: %s%s\n", oneLine(w), kindSuffix(s.Kind))
		}
	}
	return b.String()
}

// kindSuffix marks the two kinds an agent must not read past. A gateway is
// non-optional and a recovery step is the fix for the error just printed;
// KindFlow is the unmarked default because "the next useful thing" needs no
// label.
func kindSuffix(kind string) string {
	switch kind {
	case KindGateway:
		return " (required first)"
	case KindRecovery:
		return " (fixes the error above)"
	default:
		return ""
	}
}

// RenderMarkdown renders the whole graph as the website's "What to run next"
// section — the one surface where exhaustive IS appropriate, because a human is
// reading a page rather than an agent burning context.
func (g *Graph) RenderMarkdown() string {
	if g == nil || len(g.Edges) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## What to run next\n\n")

	if gw := g.GatewayMethods(); len(gw) > 0 {
		fmt.Fprintf(&b, "**Run first:** `%s` — required before the other methods work.\n\n",
			strings.Join(gw, "`, `"))
	}

	var ok, errEdges []Edge
	for _, e := range g.Edges {
		if e.On == OutcomeOK {
			ok = append(ok, e)
		} else {
			errEdges = append(errEdges, e)
		}
	}

	if len(ok) > 0 {
		b.WriteString("### After a successful call\n\n")
		for _, e := range ok {
			writeMDEdge(&b, e, describeFrom(e.From))
		}
	}
	if len(errEdges) > 0 {
		b.WriteString("### When a call fails\n\n")
		for _, e := range errEdges {
			writeMDEdge(&b, e, describeErrFrom(e))
		}
	}
	return b.String()
}

func describeFrom(from string) string {
	if from == Wildcard {
		return "After any call"
	}
	return fmt.Sprintf("After `%s`", from)
}

func describeErrFrom(e Edge) string {
	subject := "Any call"
	if e.From != Wildcard {
		subject = fmt.Sprintf("`%s`", e.From)
	}
	switch {
	case e.Code != 0:
		return fmt.Sprintf("%s fails with %d", subject, e.Code)
	case e.Match != "":
		return fmt.Sprintf("%s fails matching `%s`", subject, e.Match)
	default:
		return fmt.Sprintf("%s fails", subject)
	}
}

func writeMDEdge(b *strings.Builder, e Edge, heading string) {
	fmt.Fprintf(b, "**%s**", heading)
	if w := strings.TrimSpace(e.Why); w != "" {
		fmt.Fprintf(b, " — %s", oneLine(w))
	}
	b.WriteString("\n\n")
	for _, s := range e.Then {
		fmt.Fprintf(b, "- `%s`\n", strings.TrimSpace(s.Cmd))
		fmt.Fprintf(b, "  — %s%s\n", oneLine(s.Why), kindSuffix(s.Kind))
	}
	b.WriteString("\n")
}

// oneLine collapses whitespace so a multi-line authored string cannot break the
// single-line shape the renderers promise.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
