package publish

import (
	"net/url"
	"regexp"
	"strings"
)

var (
	vID     = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9_-]*[a-z0-9])?)+$`)
	vSemver = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$`)
	vURL    = regexp.MustCompile(`^https?://[^\s/]+`)
)

// ValidateStep checks just the fields a given wizard step owns, against the
// accumulated draft, and returns human messages. Server-authoritative: the
// handler blocks advancing while a step has errors. (HTML5 attributes give
// instant browser hints; this is the real gate.)
func ValidateStep(slug string, v url.Values) []string {
	var e []string
	switch slug {
	case "identity":
		if !vID.MatchString(strings.TrimSpace(v.Get("id"))) {
			e = append(e, "App ID must be reverse-DNS, e.g. io.pilot.myapp")
		}
		if !vSemver.MatchString(strings.TrimSpace(v.Get("app_version"))) {
			e = append(e, "Version must be semver, e.g. 0.1.0")
		}
		if strings.TrimSpace(v.Get("description")) == "" {
			e = append(e, "Description is required")
		}
	case "type":
		// TODO(native-apps): allow "cli" once binary delivery ships. See
		// docs/NATIVE-APPS.md (manifest assets + install fetch/verify/stage).
		if t := v.Get("backend_type"); t == "cli" {
			e = append(e, "Native / CLI apps are coming soon — choose HTTP API for now")
		} else if t != "http" {
			e = append(e, "Choose an app type")
		}
	case "backend":
		if v.Get("backend_type") == "cli" {
			if strings.TrimSpace(v.Get("backend_command")) == "" {
				e = append(e, "Command is required for a CLI app (e.g. mytool)")
			}
		} else if !vURL.MatchString(strings.TrimSpace(v.Get("backend_base_url"))) {
			e = append(e, "Base URL must be an absolute http(s) URL")
		}
	case "methods":
		named := false
		for _, n := range v["method_name"] {
			if strings.TrimSpace(n) != "" {
				named = true
			}
		}
		if !named {
			e = append(e, "Add at least one method")
		}
		// For an API app every named method needs a path.
		if v.Get("backend_type") != "cli" {
			names, paths := v["method_name"], v["method_path"]
			for i, n := range names {
				if strings.TrimSpace(n) != "" && strings.TrimSpace(at(paths, i)) == "" {
					e = append(e, "Method "+n+" needs a path (e.g. /search)")
				}
			}
		}
	case "listing":
		// optional — no hard requirements
	}
	return e
}
