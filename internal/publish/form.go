package publish

import (
	"net/url"
	"strings"

	"github.com/pilot-protocol/app-template/internal/scaffold"
)

// FormToConfig maps the submit form's POST values into a scaffold.Config
// (resolved, not yet validated). All parsing is server-side; the browser only
// submits raw field values. Repeated fields (method_*, header_*) are parallel
// arrays — row i across them describes one method / header.
func FormToConfig(v url.Values) (*scaffold.Config, specMeta) {
	isCLI := v.Get("backend_type") == "cli"
	cfg := &scaffold.Config{
		ID:          strings.TrimSpace(v.Get("id")),
		AppVersion:  strings.TrimSpace(v.Get("app_version")),
		Description: strings.TrimSpace(v.Get("description")),
		Backend:     scaffold.Backend{Type: "http", BaseURL: strings.TrimSpace(v.Get("backend_base_url"))},
	}
	if isCLI {
		// CLI app: the adapter execs a local command on the host. The customer
		// uploads no binary — the CLI is expected on the operator's host.
		cfg.Backend.Type = "cli"
		cfg.Backend.BaseURL = ""
		cfg.Backend.Command = strings.Fields(v.Get("backend_command"))
	} else {
		// Auth headers (name/value parallel arrays) — HTTP only.
		hNames, hVals := v["header_name"], v["header_value"]
		headers := map[string]string{}
		for i := range hNames {
			if n := strings.TrimSpace(hNames[i]); n != "" {
				headers[n] = strings.TrimSpace(at(hVals, i))
			}
		}
		if len(headers) > 0 {
			cfg.Backend.Headers = headers
		}
	}

	// Methods (parallel arrays). Empty-named rows are skipped.
	names := v["method_name"]
	verbs, paths, argv := v["method_verb"], v["method_path"], v["method_args"]
	sums, durs := v["method_summary"], v["method_duration"]
	params := v["method_params"]
	for i := range names {
		name := strings.TrimSpace(names[i])
		if name == "" {
			continue
		}
		m := scaffold.Method{Name: name, Summary: at(sums, i), Duration: orDefault(at(durs, i), "fast")}
		if isCLI {
			m.CLI = &scaffold.CLIRoute{Args: strings.Fields(at(argv, i))}
		} else {
			m.HTTP = &scaffold.HTTPRoute{Verb: orDefault(at(verbs, i), "GET"), Path: at(paths, i)}
		}
		if p := at(params, i); p != "" {
			m.Params = map[string]string{}
			for _, k := range strings.Split(p, ",") {
				if k = strings.TrimSpace(k); k != "" {
					m.Params[k] = "string"
				}
			}
		}
		cfg.Methods = append(cfg.Methods, m)
	}

	cfg.Listing = scaffold.Listing{
		DisplayName: strings.TrimSpace(v.Get("display_name")),
		Tagline:     strings.TrimSpace(v.Get("tagline")),
		Homepage:    strings.TrimSpace(v.Get("homepage")),
		SourceURL:   strings.TrimSpace(v.Get("source_url")),
		License:     strings.TrimSpace(v.Get("license")),
		Categories:  splitCSV(v.Get("categories")),
		Keywords:    splitCSV(v.Get("keywords")),
		Vendor: scaffold.Vendor{
			Name:    strings.TrimSpace(v.Get("vendor_name")),
			URL:     strings.TrimSpace(v.Get("vendor_url")),
			Contact: strings.TrimSpace(v.Get("vendor_contact")),
		},
	}

	cfg.Resolve()
	return cfg, specMeta{ID: cfg.ID, Version: cfg.AppVersion, Description: cfg.Description, DisplayName: cfg.Listing.DisplayName}
}

func at(s []string, i int) string {
	if i < len(s) {
		return strings.TrimSpace(s[i])
	}
	return ""
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
