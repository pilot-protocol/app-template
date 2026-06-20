package publish

import (
	"fmt"
	"html"
	"log"
	"os"
	"strings"

	"github.com/sendgrid/sendgrid-go"
	"github.com/sendgrid/sendgrid-go/helpers/mail"
)

// Mailer sends transactional email via SendGrid. With no SENDGRID_API_KEY set
// (local dev) it logs instead of sending, so the flow never hard-fails offline.
type Mailer struct {
	key      string
	from     string
	fromName string
	region   string // "eu" for EU data residency, else global
}

func NewMailer() *Mailer {
	return &Mailer{
		key:      os.Getenv("SENDGRID_API_KEY"),
		from:     orenv("MAIL_FROM", "apps@pilotprotocol.network"),
		fromName: orenv("MAIL_FROM_NAME", "Pilot App Store"),
		region:   strings.ToLower(os.Getenv("MAIL_REGION")),
	}
}

func orenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// Enabled reports whether a SendGrid key is configured.
func (m *Mailer) Enabled() bool { return m.key != "" }

// Send delivers one HTML email via SendGrid (text is a plain fallback). With no
// key configured (local dev) it logs instead of sending.
func (m *Mailer) Send(toEmail, subject, htmlBody, text string) error {
	if !m.Enabled() {
		log.Printf("[mail:dev] to=%s subject=%q (SENDGRID_API_KEY unset — not sent)", toEmail, subject)
		return nil
	}
	msg := mail.NewSingleEmail(mail.NewEmail(m.fromName, m.from), subject, mail.NewEmail("", toEmail), text, htmlBody)
	client := sendgrid.NewSendClient(m.key)
	if m.region == "eu" {
		if req, err := sendgrid.SetDataResidency(client.Request, "eu"); err == nil {
			client.Request = req
		}
	}
	resp, err := client.Send(msg)
	if err != nil {
		return fmt.Errorf("sendgrid send: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("sendgrid status %d: %s", resp.StatusCode, strings.TrimSpace(resp.Body))
	}
	return nil
}

// ── templates ────────────────────────────────────────────────────────────────

const (
	emailBG     = "#f7f6f1"
	emailInk    = "#15150f"
	emailDim    = "#5d5d54"
	emailAccent = "#5ea500"
	emailLine   = "#e6e5dd"
)

// shell wraps body content in a branded, email-client-safe layout.
func shell(title, body string) string {
	return `<div style="background:` + emailBG + `;padding:32px 0;font-family:'Helvetica Neue',Helvetica,Arial,sans-serif;color:` + emailInk + `">
<div style="max-width:560px;margin:0 auto;background:#fff;border:1px solid ` + emailLine + `;border-radius:14px;overflow:hidden">
  <div style="padding:22px 28px;border-bottom:1px solid ` + emailLine + `;font-size:13px;letter-spacing:.04em;text-transform:uppercase;color:` + emailDim + `">Pilot App Store</div>
  <div style="padding:28px">
    <h1 style="font-size:22px;margin:0 0 14px;color:` + emailInk + `">` + title + `</h1>
    ` + body + `
  </div>
  <div style="padding:18px 28px;border-top:1px solid ` + emailLine + `;font-size:12px;color:` + emailDim + `">Pilot Protocol · the internet for agents</div>
</div></div>`
}

// stepGraphic renders a 3-stage progress bar (Submitted → In review → Published).
// active is the 0-based index of the current/last-completed stage.
func stepGraphic(active int) string {
	stages := []string{"Submitted", "In review", "Published"}
	var cells string
	for i, s := range stages {
		dot, color, weight := "○", emailDim, "400"
		if i < active {
			dot, color, weight = "✓", emailAccent, "600"
		} else if i == active {
			dot, color, weight = "●", emailAccent, "600"
		}
		bar := ""
		if i < len(stages)-1 {
			bc := emailLine
			if i < active {
				bc = emailAccent
			}
			bar = `<td width="40" style="border-top:2px solid ` + bc + `">&nbsp;</td>`
		}
		cells += `<td align="center" style="font-size:13px;color:` + color + `;font-weight:` + weight + `;white-space:nowrap;padding:0 4px">
			<div style="font-size:18px;color:` + color + `;line-height:1">` + dot + `</div>` + s + `</td>` + bar
	}
	return `<table role="presentation" cellpadding="0" cellspacing="0" style="margin:8px 0 22px;width:100%"><tr>` + cells + `</tr></table>`
}

// submissionTable renders a read-only copy of the submitted form.
func submissionTable(s Submission) string {
	row := func(k, v string) string {
		if v == "" {
			v = "—"
		}
		return `<tr><td style="padding:8px 0;color:` + emailDim + `;width:150px;vertical-align:top;font-size:13px">` + k + `</td><td style="padding:8px 0;font-size:13px;color:` + emailInk + `">` + html.EscapeString(v) + `</td></tr>`
	}
	var methods []string
	for _, m := range s.Methods {
		methods = append(methods, html.EscapeString(m.Name+" ("+m.HTTP.Verb+" "+m.HTTP.Path+", "+m.Latency+")"))
	}
	return `<table role="presentation" cellpadding="0" cellspacing="0" style="width:100%;border-top:1px solid ` + emailLine + `;margin-top:8px">` +
		row("App ID", s.ID) + row("Version", s.Version) + row("Description", s.Description) +
		row("Backend", s.Backend.BaseURL) +
		`<tr><td style="padding:8px 0;color:` + emailDim + `;vertical-align:top;font-size:13px">Methods</td><td style="padding:8px 0;font-size:13px;color:` + emailInk + `">` + strings.Join(methods, "<br>") + `</td></tr>` +
		row("Display name", s.Listing.DisplayName) + row("License", s.Listing.License) +
		row("Vendor", s.Vendor.Name) + row("Agent usage", s.Vendor.AgentUsage) + row("Capabilities", s.Vendor.Capabilities) +
		`</table>`
}

// VerificationEmail returns (subject, html, text) for an email-verification code.
func VerificationEmail(code string) (string, string, string) {
	sub := "Your Pilot app store verification code"
	body := `<p style="color:` + emailDim + `;font-size:15px;line-height:1.5">Enter this code to verify your email and continue your submission. It expires in 10 minutes.</p>
	<div style="font-size:34px;font-weight:700;letter-spacing:.18em;margin:18px 0;color:` + emailInk + `">` + html.EscapeString(code) + `</div>
	<p style="color:` + emailDim + `;font-size:13px">If you didn't request this, you can ignore this email.</p>`
	return sub, shell("Verify your email", body), "Your Pilot app store verification code is " + code + " (expires in 10 minutes)."
}

// ConfirmationEmail confirms receipt of a submission, shows the review stage, and includes a copy.
func ConfirmationEmail(s Submission) (string, string, string) {
	sub := "We received your submission: " + s.Listing.DisplayName
	body := `<p style="color:` + emailDim + `;font-size:15px;line-height:1.5">Thanks — we've received your submission for <b style="color:` + emailInk + `">` + html.EscapeString(s.Listing.DisplayName) + `</b> (<code>` + html.EscapeString(s.ID) + `</code>). Our team builds, signs, and verifies the adapter, then reviews it. We'll email you when it's approved or if it needs changes.</p>` +
		stepGraphic(1) +
		`<h3 style="font-size:14px;margin:18px 0 0;color:` + emailInk + `">Your submission</h3>` + submissionTable(s)
	return sub, shell("Submission received", body), "We received your submission for " + s.Listing.DisplayName + " (" + s.ID + "). It is now in review."
}

// AcceptEmail tells the publisher their app is live + how to find it.
func AcceptEmail(s Submission, guide string) (string, string, string) {
	sub := "Your app is live: " + s.Listing.DisplayName
	body := `<p style="color:` + emailDim + `;font-size:15px;line-height:1.5"><b style="color:` + emailInk + `">` + html.EscapeString(s.Listing.DisplayName) + `</b> is approved and published to the Pilot app store.</p>` +
		stepGraphic(2) +
		`<h3 style="font-size:14px;margin:18px 0 6px;color:` + emailInk + `">How to find it</h3>
		<div style="color:` + emailInk + `;font-size:14px;line-height:1.6;white-space:pre-wrap">` + html.EscapeString(guide) + `</div>`
	return sub, shell("Your app is published 🎉", body), s.Listing.DisplayName + " is live on the Pilot app store.\n\n" + guide
}

// RejectEmail explains why a submission needs changes.
func RejectEmail(s Submission, reason string) (string, string, string) {
	sub := "Your submission needs changes: " + s.Listing.DisplayName
	body := `<p style="color:` + emailDim + `;font-size:15px;line-height:1.5">Thanks for submitting <b style="color:` + emailInk + `">` + html.EscapeString(s.Listing.DisplayName) + `</b>. We can't publish it as-is — here's what to address before resubmitting:</p>
	<div style="background:#fdf5f5;border:1px solid #e3b9b9;border-radius:10px;padding:14px 16px;color:#7a2222;font-size:14px;line-height:1.6;white-space:pre-wrap;margin:14px 0">` + html.EscapeString(reason) + `</div>
	<p style="color:` + emailDim + `;font-size:13px">Fix these and submit again — we're happy to take another look.</p>`
	return sub, shell("A few changes needed", body), "Your submission for " + s.Listing.DisplayName + " needs changes:\n\n" + reason
}
