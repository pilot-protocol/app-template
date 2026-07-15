package demoeval

import (
	"encoding/json"
	"os"
	"sort"
	"strings"

	"github.com/pilot-protocol/app-template/internal/demo"
	"github.com/pilot-protocol/app-template/internal/publish"
)

// FirstCallResult is the potential-uplift proxy: could a small-context agent,
// copying ONLY this demo, produce a correct first call against the app's real
// methods? It is the reproducible stand-in for "usage uplift" — no LLM required.
//
// The three bools describe the quickstart (the literal first call an install
// banner tells an agent to run); Score (0–1) folds them together with the health
// of the worked examples; Notes explain the verdict.
type FirstCallResult struct {
	// Reachable: the quickstart parses into a runnable, correctly-prefixed command
	// that targets this app's namespace.
	Reachable bool `json:"reachable"`
	// MethodValid: the method the quickstart calls is one the app actually declares.
	MethodValid bool `json:"method_valid"`
	// ArgsPlausible: every JSON arg key the quickstart passes is a declared param of
	// that method (no invented keys). A passthrough method accepts an {"args":[…]}
	// payload. Missing REQUIRED params don't flip this to false — they're recorded
	// in Notes and dock Score — because an agent can plausibly add a required arg,
	// whereas an invented key means the demo teaches a wrong call shape.
	ArgsPlausible bool `json:"args_plausible"`
	// Score in [0,1]: 0.25 reachable + 0.35 method-valid + 0.30 args-plausible +
	// 0.10 required-params-satisfied for the quickstart, scaled 0.8, plus 0.2× the
	// fraction of examples that also resolve to a valid method with plausible args.
	Score float64 `json:"score"`
	// Skipped is true when the app declares no methods (a pointer submission), so
	// the first call cannot be verified at all — distinct from a score of 0 earned
	// by a demo that references a bogus method.
	Skipped bool     `json:"skipped,omitempty"`
	Notes   []string `json:"notes,omitempty"`
}

// MethodSet is an app's callable surface: the namespace and every declared
// method with its params. It is what a first call is validated against.
type MethodSet struct {
	Namespace string
	Methods   map[string]MethodSpec
}

// MethodSpec is one declared method.
type MethodSpec struct {
	Name        string
	Params      map[string]ParamSpec
	Passthrough bool // cli passthrough: takes a verbatim {"args":[…]} payload
}

// ParamSpec is one declared parameter.
type ParamSpec struct {
	Name     string
	Required bool
}

// MethodSetFromSubmission extracts the callable surface from a parsed submission.
func MethodSetFromSubmission(s publish.Submission) MethodSet {
	ms := MethodSet{Namespace: s.Namespace(), Methods: map[string]MethodSpec{}}
	for _, m := range s.Methods {
		name := strings.TrimSpace(m.Name)
		if name == "" {
			continue
		}
		spec := MethodSpec{Name: name, Params: map[string]ParamSpec{}, Passthrough: m.CLI.Passthrough}
		for _, p := range m.Params {
			if p.Name == "" {
				continue
			}
			spec.Params[p.Name] = ParamSpec{Name: p.Name, Required: p.Required}
		}
		ms.Methods[name] = spec
	}
	return ms
}

// LoadMethodSet reads a submission.json and returns its method set. It errors on
// unreadable/unparseable JSON; a pointer submission (no methods array) yields an
// empty-but-valid method set, which SimulateFirstCall reports as unverifiable.
func LoadMethodSet(submissionPath string) (MethodSet, error) {
	raw, err := os.ReadFile(submissionPath)
	if err != nil {
		return MethodSet{}, err
	}
	var s publish.Submission
	if err := json.Unmarshal(raw, &s); err != nil {
		return MethodSet{}, err
	}
	return MethodSetFromSubmission(s), nil
}

// SimulateFirstCall runs the deterministic first-call proxy for a demo against an
// app's method set.
func SimulateFirstCall(d *demo.Demo, ms MethodSet) FirstCallResult {
	var r FirstCallResult
	if d == nil {
		r.Notes = []string{"no demo"}
		return r
	}
	if len(ms.Methods) == 0 {
		r.Skipped = true
		r.Notes = []string{"no method set for this app (pointer submission?) — first call cannot be verified"}
		return r
	}

	q := analyzeCommand(d.Quickstart.Command, ms)
	r.Reachable = q.parsed
	r.MethodValid = q.methodValid
	r.ArgsPlausible = q.parsed && q.argsParsed && len(q.unknownKeys) == 0 && q.methodValid

	var base float64
	if q.parsed {
		base += 0.25
	} else {
		r.Notes = append(r.Notes, "quickstart is not a runnable `"+strings.TrimSpace(callPrefix)+"` command")
	}
	if q.methodValid {
		base += 0.35
	} else if q.parsed {
		r.Notes = append(r.Notes, "quickstart calls method "+q.method+" which the app does not declare — a copied first call would fail")
	}
	if r.ArgsPlausible {
		base += 0.30
	} else if q.parsed && q.methodValid {
		if !q.argsParsed {
			r.Notes = append(r.Notes, "quickstart args are not valid JSON — an agent can't copy them verbatim")
		} else if len(q.unknownKeys) > 0 {
			r.Notes = append(r.Notes, "quickstart passes keys not declared by "+q.method+": "+strings.Join(q.unknownKeys, ", "))
		}
	}
	if q.methodValid && len(q.missingRequired) == 0 {
		base += 0.10
	} else if q.methodValid && len(q.missingRequired) > 0 {
		r.Notes = append(r.Notes, "quickstart omits required param(s) of "+q.method+": "+strings.Join(q.missingRequired, ", "))
	}

	// Examples: what fraction also resolve to a valid method with plausible args.
	exRatio := 1.0
	if n := len(d.Examples); n > 0 {
		valid := 0
		for _, ex := range d.Examples {
			a := analyzeCommand(ex.Command, ms)
			if a.parsed && a.methodValid && a.argsParsed && len(a.unknownKeys) == 0 {
				valid++
			}
		}
		exRatio = float64(valid) / float64(n)
		if valid < n {
			r.Notes = append(r.Notes, plural(n-valid, "example")+" reference an unknown method or invented arg keys")
		}
	}

	r.Score = round2(base*0.8 + exRatio*0.2)
	return r
}

// cmdAnalysis is the parsed+validated view of one demo command.
type cmdAnalysis struct {
	parsed          bool     // starts with the call prefix and yields app+method tokens
	appID           string   // the app id token
	method          string   // the method token
	methodValid     bool     // method is declared in the method set
	argsParsed      bool     // the JSON payload parsed into an object
	keys            []string // arg keys present in the payload
	unknownKeys     []string // keys not declared by the method (empty for passthrough)
	missingRequired []string // required params of the method not supplied
}

// analyzeCommand parses one demo command of the canonical shape
//
//	pilotctl appstore call <app-id> <ns>.<method> '<json-object>'
//
// and validates the method + arg keys against ms. The single-quoted JSON payload
// may contain spaces and escaped quotes; it is taken as everything after the
// method token, with one layer of surrounding single quotes stripped.
func analyzeCommand(command string, ms MethodSet) cmdAnalysis {
	var a cmdAnalysis
	cmd := strings.TrimSpace(command)
	if !strings.HasPrefix(cmd, callPrefix) {
		return a
	}
	rest := strings.TrimSpace(cmd[len(callPrefix):])
	appID, rest, ok := cutField(rest)
	if !ok {
		return a
	}
	method, payload, _ := cutField(rest)
	if method == "" {
		return a
	}
	a.parsed = true
	a.appID = appID
	a.method = method

	spec, ok := ms.Methods[method]
	a.methodValid = ok

	payload = strings.TrimSpace(payload)
	payload = strings.TrimPrefix(payload, "'")
	payload = strings.TrimSuffix(payload, "'")
	payload = strings.TrimSpace(payload)
	if payload == "" {
		payload = "{}"
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &obj); err == nil {
		a.argsParsed = true
		for k := range obj {
			a.keys = append(a.keys, k)
		}
		sort.Strings(a.keys)
	}

	if !a.methodValid || !a.argsParsed {
		return a
	}
	// Passthrough methods take a verbatim {"args":[…]} payload — any key that is
	// "args"/"stdin" (or a declared param) is plausible.
	for _, k := range a.keys {
		if _, declared := spec.Params[k]; declared {
			continue
		}
		if spec.Passthrough && (k == "args" || k == "stdin") {
			continue
		}
		a.unknownKeys = append(a.unknownKeys, k)
	}
	present := map[string]bool{}
	for _, k := range a.keys {
		present[k] = true
	}
	for name, p := range spec.Params {
		if p.Required && !present[name] {
			a.missingRequired = append(a.missingRequired, name)
		}
	}
	sort.Strings(a.missingRequired)
	return a
}

// cutField splits off the first whitespace-delimited token, returning it, the
// trimmed remainder, and whether a token was found.
func cutField(s string) (field, rest string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, "", true
	}
	return s[:i], strings.TrimSpace(s[i+1:]), true
}

// round2 rounds to two decimals.
func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}
