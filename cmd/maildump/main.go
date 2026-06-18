// Command maildump renders every transactional email template to an HTML file
// so they can be previewed in a browser without sending. Dev/QA tool only.
//
//	go run ./cmd/maildump -out /tmp/mail
package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"

	"github.com/pilot-protocol/app-template/internal/publish"
)

func main() {
	out := flag.String("out", "/tmp/mail", "output dir for the rendered HTML files")
	flag.Parse()
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}

	s := publish.Submission{
		ID:          "io.pilot.weather",
		Version:     "0.1.0",
		Description: "Current weather and short-term forecasts from a public API.",
		Email:       "publisher@acme.example",
		Backend:     publish.SubBackend{BaseURL: "https://api.weather.example"},
		Methods: []publish.SubMethod{
			{Name: "weather.current", Description: "Current conditions for a city.", Latency: "fast", HTTP: publish.SubRoute{Verb: "GET", Path: "/current"}},
			{Name: "weather.forecast", Description: "5-day forecast for a city.", Latency: "med", HTTP: publish.SubRoute{Verb: "GET", Path: "/forecast"}},
		},
		Listing: publish.SubListing{DisplayName: "Weather", License: "Apache-2.0"},
		Vendor: publish.SubVendor{
			Name:         "Acme Inc.",
			AgentUsage:   "Call weather.current with a city to get conditions; weather.forecast for the 5-day outlook. No setup required.",
			Capabilities: "- Current weather by city\n- 5-day forecast by city",
		},
	}

	guide := "Install it with:\n  pilotctl appstore install io.pilot.weather\n\nThen discover its methods:\n  pilotctl appstore call io.pilot.weather weather.help '{}'\n\nIt appears in the store catalogue under \"Weather\" (category: weather)."
	reason := "The backend URL https://api.weather.example was not reachable during review (connection timed out). Please confirm it is publicly accessible, then resubmit. Also, weather.forecast is missing a description of its return shape."

	files := map[string]func() (string, string, string){
		"1-verification": func() (string, string, string) { return publish.VerificationEmail("482915") },
		"2-confirmation": func() (string, string, string) { return publish.ConfirmationEmail(s) },
		"3-accept":       func() (string, string, string) { return publish.AcceptEmail(s, guide) },
		"4-reject":       func() (string, string, string) { return publish.RejectEmail(s, reason) },
	}
	for name, fn := range files {
		subject, html, _ := fn()
		page := "<!doctype html><meta charset=utf-8><title>" + subject + "</title>" + html
		p := filepath.Join(*out, name+".html")
		if err := os.WriteFile(p, []byte(page), 0o644); err != nil {
			log.Fatal(err)
		}
		log.Printf("wrote %s  (subject: %s)", p, subject)
	}
}
