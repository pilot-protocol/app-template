package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Tenancy makes ONE shared partner account behave like N isolated tenants.
//
// Some managed apps cannot give each pilot user their own partner account — e.g.
// io.pilot.agentphone, whose account is bound to a provider campaign that allows
// number generation. Upstream, every pilot user is literally the same customer.
// The partner therefore cannot enforce isolation, so the broker must.
//
// The model has exactly one rule: a caller may only touch a resource it OWNS.
//   - Creating a resource claims it (owner = creator) — see claimFrom.
//   - Referencing a resource (in the path, the query, or the body) is checked
//     against the ledger BEFORE the request is forwarded — see EnforceRequest.
//   - Listing resources filters the partner's account-wide answer down to the
//     caller's own rows — see FilterResponse.
//
// Deny-by-default is load-bearing. A resource with no ledger row belongs to
// nobody and is refused to everybody. That is what neutralises resources created
// before this shipped (the shared test number/agent): rather than being up for
// grabs, they become unreachable to every tenant.
type Tenancy struct {
	// ParamTypes maps a path-template param name to a resource type, e.g.
	// "agent_id" -> "agent". Any {param} in an allow pattern that names a
	// resource is ownership-checked.
	ParamTypes map[string]string `json:"param_types"`
	// BodyRefs maps a JSON body/query field to a resource type, e.g.
	// "agent_id" -> "agent". Path checks alone are not sufficient: an operation
	// often names the resource it acts THROUGH in the body (the sending agent on
	// a send), so an unchecked body field is an unchecked resource.
	BodyRefs map[string]string `json:"body_refs"`
	// Create declares routes that mint a resource; on 2xx the new id is claimed.
	Create []CreateRoute `json:"create"`
	// List declares routes whose response must be filtered to owned resources.
	List []ListRoute `json:"list"`
	// Delete declares routes that destroy a resource; on 2xx the claim is dropped
	// so a recycled id can be re-claimed by its next owner.
	Delete []DeleteRoute `json:"delete"`
	// Object declares routes whose response is a SUMMARY of the shared account
	// rather than a list of rows.
	Object []ObjectRoute `json:"object"`

	createIdx []compiledRoute
	listIdx   []compiledList
	deleteIdx []compiledRoute
	objectIdx []compiledObject
}

// ObjectRoute rewrites a response that describes the shared ACCOUNT instead of
// returning rows.
//
// Filtering arrays is not sufficient on a shared account: a usage/summary
// endpoint reports totals computed across every tenant. Those totals are a
// side-channel — a caller who can see none of the resources can still read how
// many exist and watch the numbers move as other tenants work.
//
// OwnedCounts replaces a count with the caller's OWN count from the ledger.
// Redact removes a field outright, for aggregates that cannot be attributed to
// one tenant at all (and so can only be wrong or leaky).
type ObjectRoute struct {
	Method      string            `json:"method"`
	Path        string            `json:"path"`
	OwnedCounts map[string]string `json:"owned_counts"` // dotted field -> resource type
	Redact      []string          `json:"redact"`       // dotted fields to drop
}

type compiledObject struct {
	method string
	segs   []string
	route  ObjectRoute
}

// CreateRoute: a 2xx on Method+Path means the caller created a resource of Type
// whose id is at IDField in the response body.
type CreateRoute struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Type    string `json:"type"`
	IDField string `json:"id_field"` // dotted path into the response, e.g. "id" or "data.id"
}

// ListRoute: the response of Method+Path carries an array at Array whose
// elements must be filtered to the caller's own.
//
// OwnerBy is why this is not simply "filter by element id": an INBOUND call or
// message is a resource the tenant never created, so it has no ledger row of its
// own — but it belongs to them because it is attached to a number they own. Each
// OwnerBy is a (field, type) link; an element is kept if ANY link resolves to a
// resource the caller owns. Elements with no resolvable link are DROPPED.
type ListRoute struct {
	Method  string      `json:"method"`
	Path    string      `json:"path"`
	Array   string      `json:"array"` // dotted path to the array, e.g. "data"
	OwnerBy []OwnerLink `json:"owner_by"`
	// ClaimAs, when set, claims each KEPT element's own id as this resource type.
	//
	// This is what makes derived resources reachable by id. A resource the partner
	// creates (an inbound call) has no ledger row of its own; it is attributable
	// only via its link to a resource the caller owns. Claiming it as it is listed
	// is what lets a later fetch-by-id be ownership-checked at all — and every
	// type named in param_types MUST be claimable somewhere, because a param with
	// no resolvable type is a param that is never checked.
	ClaimAs string `json:"claim_as"`
	// ClaimIDField is where the element's own id lives (default "id").
	ClaimIDField string `json:"claim_id_field"`
	// CountFields are sibling fields that describe the SIZE of the array (e.g.
	// "total"). They must be recomputed after filtering.
	//
	// Filtering the array alone is not enough: a count left at the partner's value
	// still reports the whole shared account, so a caller who can see none of the
	// rows can still read how many exist and watch that number move. Any field
	// derived from the unfiltered set is a leak of the unfiltered set.
	CountFields []string `json:"count_fields"`
}

// OwnerLink names a field on a list element and the resource type it points at.
type OwnerLink struct {
	Field string `json:"field"`
	Type  string `json:"type"`
}

// DeleteRoute: a 2xx on Method+Path destroyed the resource named by Param.
type DeleteRoute struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Type   string `json:"type"`
	Param  string `json:"param"` // which path param holds the id
}

type compiledRoute struct {
	method string
	segs   []string
	route  any // CreateRoute | DeleteRoute
}

type compiledList struct {
	method string
	segs   []string
	route  ListRoute
}

// compile pre-splits path templates so matching is allocation-light per request.
func (t *Tenancy) compile() {
	t.createIdx = nil
	for _, c := range t.Create {
		t.createIdx = append(t.createIdx, compiledRoute{strings.ToUpper(c.Method), strings.Split(c.Path, "/"), c})
	}
	t.listIdx = nil
	for _, l := range t.List {
		t.listIdx = append(t.listIdx, compiledList{strings.ToUpper(l.Method), strings.Split(l.Path, "/"), l})
	}
	t.deleteIdx = nil
	for _, d := range t.Delete {
		t.deleteIdx = append(t.deleteIdx, compiledRoute{strings.ToUpper(d.Method), strings.Split(d.Path, "/"), d})
	}
	t.objectIdx = nil
	for _, o := range t.Object {
		t.objectIdx = append(t.objectIdx, compiledObject{strings.ToUpper(o.Method), strings.Split(o.Path, "/"), o})
	}
}

// FilterObject rewrites an account-summary response so it describes only the
// caller. Returns (body, true) when it handled the route.
//
// Like FilterResponse it fails closed: if the body cannot be parsed or rewritten
// it returns an error document rather than passing the partner's account-wide
// answer through untouched.
func (t *Tenancy) FilterObject(s OwnerStore, app, method, path string, respBody []byte, caller string) ([]byte, bool) {
	if t == nil || s == nil {
		return respBody, false
	}
	segs := strings.Split(path, "/")
	for _, oi := range t.objectIdx {
		o := oi.route
		if oi.method != strings.ToUpper(method) || !segmentsMatch(oi.segs, segs) {
			continue
		}
		var v any
		dec := json.NewDecoder(strings.NewReader(string(respBody)))
		dec.UseNumber()
		if dec.Decode(&v) != nil {
			return []byte(`{"error":"tenancy: unfilterable upstream response"}`), true
		}
		for field, rtype := range o.OwnedCounts {
			if _, ok := dig(v, field); !ok {
				continue
			}
			owned, err := s.OwnedSet(app, rtype, caller)
			if err != nil {
				return []byte(`{"error":"tenancy: unfilterable upstream response"}`), true
			}
			if !setDug(v, field, json.Number(strconv.Itoa(len(owned)))) {
				return []byte(`{"error":"tenancy: unfilterable upstream response"}`), true
			}
		}
		for _, field := range o.Redact {
			delDug(v, field)
		}
		out, err := json.Marshal(v)
		if err != nil {
			return []byte(`{"error":"tenancy: unfilterable upstream response"}`), true
		}
		return out, true
	}
	return respBody, false
}

// delDug removes a dotted field from decoded JSON.
func delDug(v any, dotted string) {
	parts := strings.Split(dotted, ".")
	cur := v
	for i, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return
		}
		if i == len(parts)-1 {
			delete(m, p)
			return
		}
		cur = m[p]
	}
}

// pathParams extracts {name} -> value by matching path against the app's allow
// patterns. Reusing the allow patterns means a route can never be
// ownership-checked under a template that does not also admit it.
func pathParams(patterns [][]string, path string) map[string]string {
	segs := strings.Split(path, "/")
	out := map[string]string{}
	for _, pat := range patterns {
		if !segmentsMatch(pat, segs) {
			continue
		}
		for i, p := range pat {
			if len(p) > 2 && p[0] == '{' && p[len(p)-1] == '}' {
				out[p[1:len(p)-1]] = segs[i]
			}
		}
		// Keep scanning: several templates may match, and every {param} any of
		// them binds must be checked. Being permissive here would let a caller
		// pick the template with the fewest checks.
	}
	return out
}

// dig walks a dotted path ("data.id") through decoded JSON.
func dig(v any, dotted string) (any, bool) {
	cur := v
	for _, part := range strings.Split(dotted, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// asID renders a JSON scalar as a resource id. Only strings and numbers are
// ids; anything else (object, array, bool, null) is not, and returns false.
func asID(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, x != ""
	case json.Number:
		return x.String(), true
	case float64:
		// Non-integral or huge floats are not ids; avoid a lossy render.
		if x != float64(int64(x)) {
			return "", false
		}
		return json.Number(strings.TrimSuffix(strings.TrimRight(jsonNum(x), "0"), ".")).String(), true
	}
	return "", false
}

func jsonNum(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// refusal is why a request was denied. It is deliberately opaque to the caller:
// EnforceRequest's 404 must not distinguish "not yours" from "does not exist",
// or the broker becomes an oracle for enumerating other tenants' resource ids.
type refusal struct {
	Type string
	ID   string
}

// EnforceRequest is the authorization gate: it runs BEFORE the request is
// forwarded, so a rejected write never reaches the partner and never has a side
// effect. It collects every resource reference in the path, the query string,
// and the JSON body, and requires the caller to own each one.
//
// It FAILS CLOSED: an unparseable body, an unknown id, or a store error all deny.
func (t *Tenancy) EnforceRequest(s OwnerStore, app string, allowPatterns [][]string, method, path, rawQuery, contentType string, body []byte, caller string) (*refusal, bool) {
	if t == nil {
		return nil, true
	}
	if s == nil || caller == "" {
		return &refusal{}, false // no ledger or no identity → deny
	}

	// 1. Path params: /v1/agents/{agent_id}/... — the id is in the URL.
	for name, val := range pathParams(allowPatterns, path) {
		rtype, ok := t.ParamTypes[name]
		if !ok {
			continue // param names no resource (e.g. a filter) → nothing to own
		}
		if !Owns(s, app, rtype, val, caller) {
			return &refusal{Type: rtype, ID: val}, false
		}
	}

	// 2. Query params: ?agent_id=... — a filter can also be a reference.
	if rawQuery != "" {
		q, err := url.ParseQuery(rawQuery)
		if err != nil {
			return &refusal{}, false // unparseable query → deny rather than skip
		}
		for field, vals := range q {
			rtype, ok := t.BodyRefs[field]
			if !ok {
				continue
			}
			for _, v := range vals {
				if v == "" {
					continue
				}
				if !Owns(s, app, rtype, v, caller) {
					return &refusal{Type: rtype, ID: v}, false
				}
			}
		}
	}

	// 3. Body refs: {"agent_id": "..."}. An operation that names the resource it
	//    acts through in the body must be checked here, or path-level isolation
	//    is decorative.
	if len(body) > 0 && len(t.BodyRefs) > 0 {
		// A multipart body carries its refs as FORM FIELDS, not JSON keys. Without
		// this branch such a body is unparseable to the JSON decoder below and the
		// request is refused — correct, but it makes uploads impossible. Parse the
		// form instead and check the same refs against the field values.
		if mt, params, err := mime.ParseMediaType(contentType); err == nil && mt == "multipart/form-data" {
			return t.checkMultipartRefs(s, app, body, params["boundary"], caller)
		}
		// PARSER DIFFERENTIAL. The broker validates the body with Go's decoder but
		// forwards the RAW bytes, so the partner re-parses them with a different
		// parser. Go keeps the LAST duplicate key; a parser that keeps the FIRST
		// would act on a value we never checked:
		//
		//   {"agent_id":"<victim's>", "agent_id":"<mine>"}
		//
		// We would validate "<mine>" and approve; the partner would send from
		// "<victim's>". Duplicate keys have no legitimate use here, so a body
		// containing any is refused outright rather than reasoned about.
		if hasDuplicateKeys(body) {
			return &refusal{}, false
		}
		var v any
		dec := json.NewDecoder(strings.NewReader(string(body)))
		dec.UseNumber() // keep big ids exact; float64 would corrupt them
		if err := dec.Decode(&v); err != nil {
			// A body we cannot parse is a body we cannot check. If the app declares
			// body refs at all, refuse rather than forward it unchecked.
			return &refusal{}, false
		}
		// Trailing content after the first JSON value ("{...}{...}") is another way
		// two parsers can disagree about what the body says. Refuse it.
		if dec.More() {
			return &refusal{}, false
		}
		if ref, ok := t.checkRefs(s, app, v, caller, 0); !ok {
			return ref, false
		}
	}
	return nil, true
}

// maxRefDepth bounds recursion into an attacker-supplied body. A deeply nested
// body must not become a stack-exhaustion lever.
const maxRefDepth = 24

// checkRefs walks the decoded body and ownership-checks every field named in
// BodyRefs, at ANY depth. Depth matters: a ref nested inside an object must be
// checked too, or wrapping it becomes the bypass.
func (t *Tenancy) checkRefs(s OwnerStore, app string, v any, caller string, depth int) (*refusal, bool) {
	if depth > maxRefDepth {
		return &refusal{}, false // too deep to verify → deny
	}
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if rtype, ok := t.BodyRefs[k]; ok {
				if id, isID := asID(val); isID {
					if !Owns(s, app, rtype, id, caller) {
						return &refusal{Type: rtype, ID: id}, false
					}
				} else if val != nil {
					// The field names a resource but is not a scalar id (e.g. an
					// object or array). We cannot verify it → deny.
					return &refusal{Type: rtype}, false
				}
			}
			if ref, ok := t.checkRefs(s, app, val, caller, depth+1); !ok {
				return ref, false
			}
		}
	case []any:
		for _, val := range x {
			if ref, ok := t.checkRefs(s, app, val, caller, depth+1); !ok {
				return ref, false
			}
		}
	}
	return nil, true
}

// ClaimFrom records ownership after a successful create. It runs only on 2xx, so
// a failed create never claims an id.
func (t *Tenancy) ClaimFrom(s OwnerStore, app, method, path string, respBody []byte, caller string, now time.Time) {
	if t == nil || s == nil {
		return
	}
	segs := strings.Split(path, "/")
	for _, cr := range t.createIdx {
		c := cr.route.(CreateRoute)
		if cr.method != strings.ToUpper(method) || !segmentsMatch(cr.segs, segs) {
			continue
		}
		var v any
		dec := json.NewDecoder(strings.NewReader(string(respBody)))
		dec.UseNumber()
		if dec.Decode(&v) != nil {
			continue
		}
		raw, ok := dig(v, c.IDField)
		if !ok {
			continue
		}
		if id, isID := asID(raw); isID {
			_ = s.Claim(app, c.Type, id, caller, now)
		}
	}
}

// ReleaseFrom drops a claim after a successful delete, so a partner-recycled id
// can be claimed by whoever gets it next instead of staying bound to its old
// owner forever.
func (t *Tenancy) ReleaseFrom(s OwnerStore, app string, allowPatterns [][]string, method, path string) {
	if t == nil || s == nil {
		return
	}
	segs := strings.Split(path, "/")
	params := pathParams(allowPatterns, path)
	for _, dr := range t.deleteIdx {
		d := dr.route.(DeleteRoute)
		if dr.method != strings.ToUpper(method) || !segmentsMatch(dr.segs, segs) {
			continue
		}
		if id := params[d.Param]; id != "" {
			_ = s.Release(app, d.Type, id)
		}
	}
}

// FilterResponse rewrites a list response to contain only the caller's own
// resources. This is what makes "pilot users must not see numbers other than
// their own" true: the partner answers for the whole shared account, and the
// broker strips it to the caller's rows before it ever reaches them.
//
// It also CLAIMS kept elements (lazy claim). Inbound calls/messages are created
// by the partner, not the tenant, so they have no row yet; claiming them when
// they are provably linked to an owned number is what lets the tenant then fetch
// them by id.
//
// On any doubt it returns an EMPTY array rather than the unfiltered body:
// leaking another tenant's rows is worse than showing none.
func (t *Tenancy) FilterResponse(s OwnerStore, app, method, path string, respBody []byte, caller string, now time.Time) ([]byte, bool) {
	if t == nil || s == nil {
		return respBody, false
	}
	segs := strings.Split(path, "/")
	for _, li := range t.listIdx {
		l := li.route
		if li.method != strings.ToUpper(method) || !segmentsMatch(li.segs, segs) {
			continue
		}
		var v any
		dec := json.NewDecoder(strings.NewReader(string(respBody)))
		dec.UseNumber()
		if dec.Decode(&v) != nil {
			// Cannot parse → cannot filter → do not pass the partner's raw
			// account-wide answer through.
			return []byte(`{"error":"tenancy: unfilterable upstream response"}`), true
		}
		arrRaw, ok := dig(v, l.Array)
		if !ok {
			return respBody, false // no array here; nothing to filter
		}
		arr, ok := arrRaw.([]any)
		if !ok {
			return []byte(`{"error":"tenancy: unfilterable upstream response"}`), true
		}
		kept := make([]any, 0, len(arr))
		for _, el := range arr {
			if t.keepElement(s, app, el, caller, l.OwnerBy, l.ClaimAs, l.ClaimIDField, now) {
				kept = append(kept, el)
			}
		}
		if !setDug(v, l.Array, kept) {
			return []byte(`{"error":"tenancy: unfilterable upstream response"}`), true
		}
		// Recompute any count that described the unfiltered set. Note that
		// pagination flags (hasMore) are deliberately left alone: they describe the
		// partner's underlying paging, and forcing them false would stop a client
		// paging before it reached its OWN rows on a later page.
		for _, cf := range l.CountFields {
			if _, ok := dig(v, cf); ok {
				_ = setDug(v, cf, json.Number(strconv.Itoa(len(kept))))
			}
		}
		out, err := json.Marshal(v)
		if err != nil {
			return []byte(`{"error":"tenancy: unfilterable upstream response"}`), true
		}
		return out, true
	}
	return respBody, false
}

// keepElement decides if a list element belongs to caller, and lazily claims it.
func (t *Tenancy) keepElement(s OwnerStore, app string, el any, caller string, links []OwnerLink, claimAs, claimIDField string, now time.Time) bool {
	m, ok := el.(map[string]any)
	if !ok {
		return false // not an object → cannot attribute → drop
	}
	for _, link := range links {
		raw, ok := m[link.Field]
		if !ok {
			continue
		}
		id, isID := asID(raw)
		if !isID {
			continue
		}
		if Owns(s, app, link.Type, id, caller) {
			// Provably the caller's. Claim the element itself so a later
			// fetch-by-id can be ownership-checked at all.
			if claimAs != "" {
				field := claimIDField
				if field == "" {
					field = "id"
				}
				if selfID, ok := asID(m[field]); ok && selfID != "" {
					_ = s.Claim(app, claimAs, selfID, caller, now)
				}
			}
			return true
		}
	}
	return false
}

// setDug writes a value at a dotted path in decoded JSON.
func setDug(v any, dotted string, val any) bool {
	parts := strings.Split(dotted, ".")
	cur := v
	for i, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		if i == len(parts)-1 {
			m[p] = val
			return true
		}
		cur, ok = m[p]
		if !ok {
			return false
		}
	}
	return false
}

// validateTenancy rejects a tenancy block that would not actually isolate.
//
// This runs at registry load and FAILS THE BOOT rather than warning. A tenancy
// spec is a security control: a typo that silently disables a check is the whole
// bug class we are fixing, so a broken spec must never start serving traffic.
func validateTenancy(a *AppEntry) error {
	t := a.Tenancy
	if len(t.ParamTypes) == 0 && len(t.BodyRefs) == 0 {
		return fmt.Errorf("registry: app %s: tenancy declares no param_types or body_refs — it would enforce nothing", a.ID)
	}
	types := map[string]bool{}
	for _, ty := range t.ParamTypes {
		types[ty] = true
	}
	for _, ty := range t.BodyRefs {
		types[ty] = true
	}
	for _, c := range t.Create {
		if c.Type == "" || c.Path == "" || c.Method == "" || c.IDField == "" {
			return fmt.Errorf("registry: app %s: tenancy.create needs method, path, type and id_field", a.ID)
		}
		types[c.Type] = true
	}
	for _, d := range t.Delete {
		if d.Type == "" || d.Path == "" || d.Method == "" || d.Param == "" {
			return fmt.Errorf("registry: app %s: tenancy.delete needs method, path, type and param", a.ID)
		}
	}
	for _, l := range t.List {
		if l.Path == "" || l.Array == "" || len(l.OwnerBy) == 0 {
			return fmt.Errorf("registry: app %s: tenancy.list %q needs array and at least one owner_by link (an unfiltered list leaks every tenant)", a.ID, l.Path)
		}
		for _, link := range l.OwnerBy {
			if link.Field == "" || link.Type == "" {
				return fmt.Errorf("registry: app %s: tenancy.list %q has an owner_by with an empty field/type", a.ID, l.Path)
			}
			if !types[link.Type] {
				return fmt.Errorf("registry: app %s: tenancy.list %q links to resource type %q that nothing ever claims — every element would be dropped", a.ID, l.Path, link.Type)
			}
		}
	}
	// A multipart upload names the resource it acts on in a FORM FIELD, never in
	// the path — POST /api/v1/documents carries deal_id in the body and nowhere
	// else. So an app that forwards multipart while declaring no body_refs has
	// no way to check the one reference that matters, and every caller could
	// upload into every other caller's resource. param_types alone satisfies the
	// check above, which is exactly how this would slip through unnoticed.
	if len(t.BodyRefs) == 0 && a.forwardsMultipart() {
		return fmt.Errorf("registry: app %s: forwards multipart/form-data but declares no tenancy.body_refs — an upload names its resource in a form field, so nothing would be ownership-checked", a.ID)
	}

	// Every claimable type should be reachable: a type referenced by params/body
	// but never created can never be owned, which would deny the app entirely.
	created := map[string]bool{}
	for _, c := range t.Create {
		created[c.Type] = true
	}
	for _, ty := range t.ParamTypes {
		if !created[ty] && !t.lazyClaimed(ty) {
			return fmt.Errorf("registry: app %s: tenancy resource %q is referenced but never created or list-claimed — nobody could ever own it", a.ID, ty)
		}
	}
	return nil
}

// lazyClaimed reports whether a type can be claimed from a list response (the
// inbound-resource path), so it need not have an explicit create route.
func (t *Tenancy) lazyClaimed(rtype string) bool {
	for _, l := range t.List {
		if l.ClaimAs == rtype {
			return true
		}
	}
	return false
}

// hasDuplicateKeys reports whether any JSON object in body repeats a key.
//
// This exists purely to kill parser-differential bypasses (see EnforceRequest).
// It walks the TOKEN stream, not the decoded value: by the time a value is
// decoded the duplicate is already gone, because Go silently kept the last one.
//
// It fails closed — a body that is malformed or unbalanced counts as duplicated.
// If we cannot prove the body says exactly one thing, we do not forward it.
func hasDuplicateKeys(body []byte) bool {
	type frame struct {
		keys      map[string]bool // nil for arrays
		expectVal bool            // an object key was read; the next token is its value
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	var stack []*frame

	// valueConsumed marks that the enclosing object's pending key now has its
	// value. It must run for EVERY kind of value — including a nested object or
	// array — or the key that follows the nested value is mistaken for a value
	// and its duplicate goes unnoticed.
	valueConsumed := func() {
		if n := len(stack); n > 0 && stack[n-1].keys != nil {
			stack[n-1].expectVal = false
		}
	}

	for {
		tok, err := dec.Token()
		if err != nil {
			// Clean end only counts if every container closed.
			if errors.Is(err, io.EOF) {
				return len(stack) != 0
			}
			return true // malformed → ambiguous → refuse
		}
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{':
				valueConsumed()
				stack = append(stack, &frame{keys: map[string]bool{}})
			case '[':
				valueConsumed()
				stack = append(stack, &frame{})
			case '}', ']':
				if len(stack) == 0 {
					return true // unbalanced
				}
				stack = stack[:len(stack)-1]
			}
		case string:
			n := len(stack)
			if n == 0 {
				continue // top-level scalar
			}
			top := stack[n-1]
			if top.keys != nil && !top.expectVal {
				// Directly inside an object and not awaiting a value → this is a KEY.
				if top.keys[v] {
					return true
				}
				top.keys[v] = true
				top.expectVal = true
				continue
			}
			valueConsumed()
		default:
			valueConsumed()
		}
	}
}

// Multipart parsing bounds. A body that needs more than this to describe itself
// is not a legitimate upload; refusing is cheaper than reasoning about it.
const (
	maxMultipartParts = 64
	maxRefFieldBytes  = 4 << 10 // a resource id is never larger
)

// checkMultipartRefs ownership-checks the BodyRefs fields of a multipart body.
//
// It is the multipart twin of the JSON body check and keeps the same three
// properties, because a shared master key sits behind it:
//
//   - FAIL-CLOSED. A body that cannot be parsed, a boundary that is missing, a
//     part budget that is exceeded — all deny. We never forward a body whose
//     refs we could not read.
//   - NO PARSER DIFFERENTIAL on a checked field. A REF field that appears twice
//     is refused: Go hands us every part in order, other stacks keep the first
//     or the last, so a body naming deal_id twice could be validated as one
//     value and acted on as another. Repeats of fields we do not check are
//     allowed through — they are legal multipart (a multi-file upload posts the
//     same name repeatedly) and no security decision rests on them, so banning
//     them would break real apps to no end.
//   - A REF IS A FIELD, NEVER A FILE. A part that carries a filename is treated
//     as file content and its bytes are not inspected. So a caller who smuggles
//     "deal_id" in as a file part would slip an unchecked ref past us if we
//     simply skipped it — that shape is denied explicitly.
func (t *Tenancy) checkMultipartRefs(s OwnerStore, app string, body []byte, boundary, caller string) (*refusal, bool) {
	if boundary == "" {
		return &refusal{}, false // no boundary → undecodable → deny
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	type ref struct {
		rtype string
		value string
	}
	found := map[string]ref{}
	parts := 0
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return &refusal{}, false // malformed → cannot verify → deny
		}
		// Count only parts we actually received. Checking the budget before
		// NextPart rejects a body sitting exactly on the limit, because the call
		// that would have reported EOF never happens.
		parts++
		if parts > maxMultipartParts {
			part.Close()
			return &refusal{}, false // more parts than any real upload needs
		}
		name := part.FormName()
		rtype, isRef := t.BodyRefs[name]
		if !isRef {
			// Not a field we check. Close advances past the part's remaining
			// bytes; NextPart would do it anyway, so there is nothing to drain
			// by hand.
			part.Close()
			continue
		}
		if part.FileName() != "" {
			// A ref wearing a filename: file parts are not inspected, so this is
			// the shape that routes a ref past the check.
			part.Close()
			return &refusal{}, false
		}
		if _, dup := found[name]; dup {
			part.Close()
			return &refusal{}, false // repeated REF → parser differential → deny
		}
		val, err := io.ReadAll(io.LimitReader(part, maxRefFieldBytes+1))
		part.Close()
		if err != nil || len(val) > maxRefFieldBytes {
			return &refusal{}, false
		}
		found[name] = ref{rtype: rtype, value: strings.TrimSpace(string(val))}
	}
	for _, r := range found {
		// An empty ref names no resource — the field is present but unset, which
		// the partner treats as absent. Matches the JSON path, where asID rejects
		// the empty string and the ref is skipped rather than checked.
		if r.value == "" {
			continue
		}
		if !Owns(s, app, r.rtype, r.value, caller) {
			return &refusal{Type: r.rtype, ID: r.value}, false
		}
	}
	return nil, true
}
