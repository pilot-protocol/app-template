package scaffold

import (
	"encoding/json"
	"strings"
)

// descOr returns the long-form app description when set, else the one-line
// description. This is what the store page (`appstore view`) renders.
func descOr(long, short string) string {
	if strings.TrimSpace(long) != "" {
		return long
	}
	return short
}

// Metadata is the per-app catalogue v2 record (catalogue/apps/<id>/metadata.json)
// that drives the store rich-view page. Built from the spec at `init`; the
// runtime facts (publisher pubkey, sizes, timestamps) are filled at submit time.
type Metadata struct {
	SchemaVersion int            `json:"schema_version"`
	ID            string         `json:"id"`
	DisplayName   string         `json:"display_name"`
	Tagline       string         `json:"tagline,omitempty"`
	DescriptionMD string         `json:"description_md"`
	Vendor        MetaVendor     `json:"vendor"`
	Homepage      string         `json:"homepage,omitempty"`
	SourceURL     string         `json:"source_url,omitempty"`
	License       string         `json:"license,omitempty"`
	Categories    []string       `json:"categories,omitempty"`
	Keywords      []string       `json:"keywords,omitempty"`
	Size          MetaSize       `json:"size"`
	Compat        MetaCompat     `json:"compat"`
	Methods       []MetaMethod   `json:"methods"`
	Changelog     []ChangelogRel `json:"changelog,omitempty"`
	Links         []MetaLink     `json:"links,omitempty"`
	PublishedAt   string         `json:"published_at,omitempty"`
	UpdatedAt     string         `json:"updated_at,omitempty"`
}

type MetaVendor struct {
	Name            string `json:"name"`
	URL             string `json:"url,omitempty"`
	Contact         string `json:"contact,omitempty"`
	PublisherPubkey string `json:"publisher_pubkey,omitempty"`
}

type MetaSize struct {
	BundleBytes    int64 `json:"bundle_bytes"`
	InstalledBytes int64 `json:"installed_bytes"`
}

type MetaCompat struct {
	MinPilotVersion string   `json:"min_pilot_version"`
	Runtimes        []string `json:"runtimes"`
}

type MetaMethod struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
}

type MetaLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// BuildMetadata constructs the static metadata for cfg. Runtime facts
// (Vendor.PublisherPubkey, Size, timestamps) are zero/empty here and filled by
// `pilot-app submit` once the bundle is built + signed.
func BuildMetadata(c *Config) Metadata {
	methods := make([]MetaMethod, 0, len(c.Methods)+1)
	for _, m := range c.Methods {
		methods = append(methods, MetaMethod{Name: m.Name, Summary: m.Summary})
	}
	methods = append(methods, MetaMethod{
		Name:    c.Namespace + ".help",
		Summary: "Discovery: every method with params, kind, and latency class.",
	})

	changelog := c.Listing.Changelog
	if len(changelog) == 0 {
		changelog = []ChangelogRel{{Version: c.AppVersion, Notes: []string{c.Description}}}
	}

	// Managed apps require a daemon that provisions a per-app identity (--identity)
	// and grants key.sign — features that landed in 1.10.0. BYO apps work on older
	// daemons.
	minPilot := "1.0.0"
	if c.Managed() {
		minPilot = "1.10.0"
	}

	var links []MetaLink
	if c.Listing.SourceURL != "" {
		links = append(links, MetaLink{Label: "Source", URL: c.Listing.SourceURL})
	}
	if c.Listing.Homepage != "" {
		links = append(links, MetaLink{Label: "Website", URL: c.Listing.Homepage})
	}

	return Metadata{
		SchemaVersion: 1,
		ID:            c.ID,
		DisplayName:   c.Listing.DisplayName,
		Tagline:       c.Listing.Tagline,
		DescriptionMD: descOr(c.Listing.AppDescription, c.Description),
		Vendor: MetaVendor{
			Name:    c.Listing.Vendor.Name,
			URL:     c.Listing.Vendor.URL,
			Contact: c.Listing.Vendor.Contact,
		},
		Homepage:   c.Listing.Homepage,
		SourceURL:  c.Listing.SourceURL,
		License:    c.Listing.License,
		Categories: c.Listing.Categories,
		Keywords:   c.Listing.Keywords,
		Compat:     MetaCompat{MinPilotVersion: minPilot, Runtimes: []string{"go"}},
		Methods:    methods,
		Changelog:  changelog,
		Links:      links,
	}
}

// marshalMetadata renders metadata.json with stable, human-readable formatting.
func marshalMetadata(m Metadata) ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
