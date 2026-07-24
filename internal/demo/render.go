package demo

import (
	"fmt"
	"strings"
)

// RenderSkill renders the demo in skill-file format: YAML frontmatter
// (name + when_to_use — what lets a small-context agent decide WHEN to reach
// for the app) followed by the WHAT-to-run body. It is a library helper for
// harnesses that consume skills; it is NOT auto-injected at install time.
// Deterministic output — safe to diff and test.
func (d *Demo) RenderSkill(appID string) string {
	var b strings.Builder
	desc := fmt.Sprintf("%s — %s", d.TitleOr(), d.WhenToUse)
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", appID)
	fmt.Fprintf(&b, "description: %s\n", oneLine(desc))
	fmt.Fprintf(&b, "when_to_use: %s\n", oneLine(d.WhenToUse))
	b.WriteString("---\n\n")

	fmt.Fprintf(&b, "# %s — %s\n\n", appID, d.TitleOr())
	fmt.Fprintf(&b, "**When to use:** %s\n\n", d.WhenToUse)

	b.WriteString("## Run this first\n\n")
	d.writeStep(&b, d.Quickstart, d.Metered)

	b.WriteString("## Worked examples\n\n")
	for _, ex := range d.Examples {
		d.writeStep(&b, ex, d.Metered)
	}

	if d.Metered && d.Cost != nil {
		d.writeCost(&b)
	}
	if len(d.Gotchas) > 0 {
		b.WriteString("## Gotchas\n\n")
		for _, g := range d.Gotchas {
			fmt.Fprintf(&b, "- %s\n", g)
		}
		b.WriteString("\n")
	}
	if len(d.Next) > 0 {
		b.WriteString("## Go deeper\n\n")
		for _, n := range d.Next {
			fmt.Fprintf(&b, "- %s\n", n)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// RenderInstall produces the compact block pilotctl prints at the last step of
// install. Same content as the skill, minus the frontmatter, tuned for a
// terminal: a headline, the first call, the examples, and (metered) the budget.
func (d *Demo) RenderInstall(appID string) string {
	var b strings.Builder
	bar := strings.Repeat("─", 64)
	fmt.Fprintf(&b, "%s\n", bar)
	fmt.Fprintf(&b, "  %s installed — %s\n", appID, d.TitleOr())
	fmt.Fprintf(&b, "%s\n\n", bar)
	fmt.Fprintf(&b, "  When to use: %s\n\n", d.WhenToUse)

	b.WriteString("  ▶ Run this first:\n")
	fmt.Fprintf(&b, "    %s\n", d.Quickstart.Command)
	if d.Quickstart.Expect != "" {
		fmt.Fprintf(&b, "    → %s\n", d.Quickstart.Expect)
	}
	b.WriteString("\n")

	b.WriteString("  ▶ More examples:\n")
	for _, ex := range d.Examples {
		if ex.Title != "" {
			fmt.Fprintf(&b, "    # %s%s\n", ex.Title, costSuffix(d.Metered, ex.Cost))
		}
		fmt.Fprintf(&b, "    %s\n", ex.Command)
	}
	b.WriteString("\n")

	if d.Metered && d.Cost != nil {
		fmt.Fprintf(&b, "  ▶ Budget: %s. ", strings.TrimSpace(d.Cost.FreeBudget))
		if d.Cost.WorkedTotal != "" {
			fmt.Fprintf(&b, "%s\n", d.Cost.WorkedTotal)
		} else {
			b.WriteString("Reads are free; see the price table above.\n")
		}
		if d.Cost.CheckBalance != "" {
			fmt.Fprintf(&b, "    balance: %s\n", d.Cost.CheckBalance)
		}
		b.WriteString("\n")
	}
	if len(d.Next) > 0 {
		fmt.Fprintf(&b, "  Full reference: %s\n", strings.Join(d.Next, " · "))
	}
	fmt.Fprintf(&b, "%s\n", bar)
	return b.String()
}

// RenderMarkdown produces the website "Full usage demo" section: a plain,
// self-contained Markdown block the Astro page (and its plain twin) can embed.
func (d *Demo) RenderMarkdown(appID string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n\n", d.TitleOr())
	fmt.Fprintf(&b, "*When to use:* %s\n\n", d.WhenToUse)

	b.WriteString("**Run this first**\n\n")
	d.writeStep(&b, d.Quickstart, d.Metered)

	b.WriteString("**Examples**\n\n")
	for _, ex := range d.Examples {
		d.writeStep(&b, ex, d.Metered)
	}
	if d.Metered && d.Cost != nil {
		d.writeCost(&b)
	}
	if len(d.Gotchas) > 0 {
		b.WriteString("**Gotchas**\n\n")
		for _, g := range d.Gotchas {
			fmt.Fprintf(&b, "- %s\n", g)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (d *Demo) writeStep(b *strings.Builder, s Step, metered bool) {
	head := s.Title
	if head == "" {
		head = s.Goal
	}
	if head != "" {
		fmt.Fprintf(b, "*%s%s*\n\n", head, costSuffix(metered, s.Cost))
	}
	fmt.Fprintf(b, "```bash\n%s\n```\n", s.Command)
	if s.Expect != "" {
		fmt.Fprintf(b, "→ `%s`\n", oneLine(s.Expect))
	}
	if s.Note != "" {
		fmt.Fprintf(b, "\n%s\n", s.Note)
	}
	b.WriteString("\n")
}

func (d *Demo) writeCost(b *strings.Builder) {
	c := d.Cost
	b.WriteString("## Cost\n\n")
	fmt.Fprintf(b, "Free budget: **%s**. Unit: %s.\n\n", strings.TrimSpace(c.FreeBudget), c.Unit)
	if len(c.Operations) > 0 {
		b.WriteString("| Operation | Price | Notes |\n|---|---|---|\n")
		for _, op := range c.Operations {
			fmt.Fprintf(b, "| `%s` | %s | %s |\n", op.Op, op.Price, op.Note)
		}
		b.WriteString("\n")
	}
	if c.WorkedTotal != "" {
		fmt.Fprintf(b, "%s\n\n", c.WorkedTotal)
	}
	if c.CheckBalance != "" {
		fmt.Fprintf(b, "Check your balance: `%s`\n\n", c.CheckBalance)
	}
}

func costSuffix(metered bool, cost string) string {
	if !metered || strings.TrimSpace(cost) == "" {
		return ""
	}
	return "  (" + strings.TrimSpace(cost) + ")"
}

// oneLine collapses newlines so a value is safe inside YAML frontmatter or a
// single Markdown line.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}
