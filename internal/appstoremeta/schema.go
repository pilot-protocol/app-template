// SPDX-License-Identifier: AGPL-3.0-or-later

// Package appstoremeta is the canonical app-store presentation schema and the
// read-only API that serves it.
//
// Presentation is not the catalogue. The publisher-signed catalogue decides
// what exists and what may be installed; this package only decides how an app
// is described. Nothing served here grants a capability, and a consumer that
// cannot reach this API must degrade to its own last-known copy rather than
// fail — both the console and the public site are built that way.
package appstoremeta

// SchemaVersion is the contract version of the document served at
// /v1/appstore/metadata. It changes only when a field is removed or its
// meaning changes; adding a field does not bump it, because both consumers
// decode into their own structs and ignore what they do not know.
const SchemaVersion = 1

// Document is the whole presentation snapshot: the shelves, the carousel
// order, and every app.
type Document struct {
	SchemaVersion int        `json:"schema_version"`
	GeneratedAt   string     `json:"generated_at"`
	Source        string     `json:"source"`
	Categories    []Category `json:"categories"`
	FeaturedOrder []string   `json:"featured_order"`
	AppOrder      []string   `json:"app_order"`
	Apps          []App      `json:"apps"`
}

// Category is a shelf heading. Hue drives the fallback lettermark plate for
// apps with no icon, so it is presentation, not taxonomy.
type Category struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Blurb string `json:"blurb"`
	Hue   int    `json:"hue"`
}

// App is the complete presentation record for one app: the superset both the
// public site and the management console render.
//
// Description is the authored Markdown and is the only long-copy field that
// is edited by hand. Summary is derived from it (see Summarize) so the two
// surfaces cannot drift: previously the console carried a hand-flattened copy
// that had to be re-pasted whenever the site's copy changed.
type App struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Tagline     string `json:"tagline"`
	Description string `json:"description"`
	Summary     string `json:"summary"`

	Categories      []string `json:"categories"`
	PrimaryCategory string   `json:"primary_category"`
	Keywords        []string `json:"keywords"`

	Version   string `json:"version"`
	Vendor    string `json:"vendor"`
	VendorURL string `json:"vendor_url"`
	License   string `json:"license"`
	SourceURL string `json:"source_url"`
	Homepage  string `json:"homepage"`

	Methods   []Method  `json:"methods"`
	Changelog []Release `json:"changelog"`
	Grants    []string  `json:"grants"`
	Limits    []Limit   `json:"limits"`

	Bundles        []Bundle     `json:"bundles"`
	InstalledBytes int64        `json:"installed_bytes"`
	Depends        []Dependency `json:"depends"`

	Protection  string `json:"protection"`
	Featured    bool   `json:"featured"`
	InCatalogue bool   `json:"in_catalogue"`

	Icon            Icon     `json:"icon"`
	MinPilotVersion string   `json:"min_pilot_version"`
	Runtimes        []string `json:"runtimes"`

	PublishedAt string `json:"published_at"`
	UpdatedAt   string `json:"updated_at"`

	ProductDemo *ProductDemo `json:"product_demo"`
}

// Method is one typed IPC method the app exposes. Gated names the credential
// or state a call needs ("api key", "after signup"); empty means ungated.
type Method struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
	Example string `json:"example"`
	Gated   string `json:"gated"`
}

// Release is one published version and what changed in it.
type Release struct {
	Version string   `json:"version"`
	Date    string   `json:"date"`
	Notes   []string `json:"notes"`
}

// Limit is a published quota or ceiling, rendered as a label/value pair.
type Limit struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Bundle is one published platform build and its download size.
type Bundle struct {
	Platform string `json:"platform"`
	Bytes    int64  `json:"bytes"`
}

// Dependency is another app this one calls.
type Dependency struct {
	ID       string `json:"id"`
	Reason   string `json:"reason"`
	Optional bool   `json:"optional"`
}

// Icon is how an app's mark is drawn. Mode "image" renders Img; mode "mask"
// tints File as a mask over the Color plate. Ink says the plate is light
// enough to need dark ink.
type Icon struct {
	Mode  string    `json:"mode"`
	Img   string    `json:"img"`
	File  string    `json:"file"`
	Fit   string    `json:"fit"`
	Pos   string    `json:"pos"`
	Color string    `json:"color"`
	Ink   bool      `json:"ink"`
	Hue   int       `json:"hue"`
	Mark  *IconMark `json:"mark"`
}

// IconMark names the upstream glyph a mask icon is cut from, so a consumer
// that builds its own icon assets knows which one to take.
//
// Without it the website's build had to keep its own id-to-glyph table, which
// is exactly the second hardcoded location this API exists to remove.
type IconMark struct {
	Set  string `json:"set"`  // "simple-icons" or "lucide"
	Name string `json:"name"` // the file's base name in that set
}

// ProductDemo is the example-first usage guide shown at install and rendered
// on both surfaces. Its shape matches the product_demo block authored in an
// app-template submission.
type ProductDemo struct {
	Skill      string     `json:"skill"`
	Title      string     `json:"title"`
	WhenToUse  string     `json:"when_to_use"`
	Metered    bool       `json:"metered"`
	Quickstart DemoStep   `json:"quickstart"`
	Examples   []DemoStep `json:"examples"`
	Cost       *DemoCost  `json:"cost"`
	Gotchas    []string   `json:"gotchas"`
	Next       []string   `json:"next"`
}

// DemoStep is one runnable command in a product demo.
type DemoStep struct {
	Title   string `json:"title"`
	Goal    string `json:"goal"`
	Command string `json:"command"`
	Expect  string `json:"expect"`
	Cost    string `json:"cost"`
	Note    string `json:"note"`
}

// DemoCost is what a metered app charges, and the free budget before it does.
type DemoCost struct {
	Unit         string       `json:"unit"`
	FreeBudget   string       `json:"free_budget"`
	HardCapUSD   *float64     `json:"hard_cap_usd"`
	Operations   []DemoCostOp `json:"operations"`
	WorkedTotal  string       `json:"worked_total"`
	CheckBalance string       `json:"check_balance"`
}

// DemoCostOp prices one operation.
type DemoCostOp struct {
	Op    string `json:"op"`
	Price string `json:"price"`
	Note  string `json:"note"`
}
