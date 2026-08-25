// SPDX-License-Identifier: AGPL-3.0-or-later

package appstoremeta

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// indexFile and appsDir are the layout of a data directory: one index naming
// the shelves and the carousel, and one file per app named for its id.
//
// One file per app is what makes this data reviewable. A single large document
// produced a diff nobody could read whenever one app's copy changed, which is
// how the console's copy came to drift from the site's in the first place.
const (
	indexFile = "index.json"
	appsDir   = "apps"
)

// knownProtections and knownIconModes are closed vocabularies. Both consumers
// switch on these values, so an unknown one renders as nothing at all rather
// than as an obvious error — worth failing the load for.
var (
	knownProtections = map[string]bool{"guarded": true, "shareable": true}
	knownIconModes   = map[string]bool{"mask": true, "image": true}
	knownIconSets    = map[string]bool{"simple-icons": true, "lucide": true}
)

type indexDocument struct {
	SchemaVersion int        `json:"schema_version"`
	Source        string     `json:"source"`
	Categories    []Category `json:"categories"`
	FeaturedOrder []string   `json:"featured_order"`
}

// App returns one app by id.
func (document *Document) App(id string) (App, bool) {
	for _, app := range document.Apps {
		if app.ID == id {
			return app, true
		}
	}
	return App{}, false
}

// Load reads and validates a data directory, deriving every computed field.
//
// It is strict on purpose. This document is the only description of an app
// that either surface has, so a typo that silently drops a section is worse
// than a server that refuses to start: the operator sees the failure at deploy
// time instead of discovering a blank app page in production.
func Load(files fs.FS) (*Document, error) {
	index, err := loadIndex(files)
	if err != nil {
		return nil, err
	}

	entries, err := fs.ReadDir(files, appsDir)
	if err != nil {
		return nil, fmt.Errorf("read %s/: %w", appsDir, err)
	}

	categories := make(map[string]bool, len(index.Categories))
	for _, category := range index.Categories {
		categories[category.ID] = true
	}

	apps := make([]App, 0, len(entries))
	seen := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(path.Ext(entry.Name()), ".json") {
			continue
		}
		app, err := loadApp(files, entry.Name(), categories)
		if err != nil {
			return nil, err
		}
		if previous, clash := seen[app.ID]; clash {
			return nil, fmt.Errorf("%s/%s: app id %q already defined by %s", appsDir, entry.Name(), app.ID, previous)
		}
		seen[app.ID] = entry.Name()
		apps = append(apps, app)
	}
	if len(apps) == 0 {
		return nil, fmt.Errorf("%s/: no app files found", appsDir)
	}

	// Sorted so the served bytes — and therefore the ETag — depend only on the
	// content, never on directory order.
	sort.Slice(apps, func(i, j int) bool { return apps[i].ID < apps[j].ID })

	for _, id := range index.FeaturedOrder {
		if _, ok := seen[id]; !ok {
			return nil, fmt.Errorf("%s: featured_order names %q, which has no app file", indexFile, id)
		}
	}

	return &Document{
		SchemaVersion: SchemaVersion,
		Source:        index.Source,
		Categories:    index.Categories,
		FeaturedOrder: nonNilStrings(index.FeaturedOrder),
		Apps:          apps,
	}, nil
}

func loadIndex(files fs.FS) (indexDocument, error) {
	var index indexDocument
	body, err := fs.ReadFile(files, indexFile)
	if err != nil {
		return index, fmt.Errorf("read %s: %w", indexFile, err)
	}
	if err := json.Unmarshal(body, &index); err != nil {
		return index, fmt.Errorf("parse %s: %w", indexFile, err)
	}
	if len(index.Categories) == 0 {
		return index, fmt.Errorf("%s: at least one category is required", indexFile)
	}
	seen := make(map[string]bool, len(index.Categories))
	for _, category := range index.Categories {
		switch {
		case category.ID == "":
			return index, fmt.Errorf("%s: a category has no id", indexFile)
		case category.Name == "":
			return index, fmt.Errorf("%s: category %q has no name", indexFile, category.ID)
		case seen[category.ID]:
			return index, fmt.Errorf("%s: duplicate category id %q", indexFile, category.ID)
		case category.Hue < 0 || category.Hue > 360:
			return index, fmt.Errorf("%s: category %q hue %d is outside 0-360", indexFile, category.ID, category.Hue)
		}
		seen[category.ID] = true
	}
	return index, nil
}

func loadApp(files fs.FS, name string, categories map[string]bool) (App, error) {
	var app App
	where := appsDir + "/" + name

	body, err := fs.ReadFile(files, where)
	if err != nil {
		return app, fmt.Errorf("read %s: %w", where, err)
	}
	if err := json.Unmarshal(body, &app); err != nil {
		return app, fmt.Errorf("parse %s: %w", where, err)
	}
	if err := validateApp(&app, where, strings.TrimSuffix(name, path.Ext(name)), categories); err != nil {
		return app, err
	}

	app.Summary = Summarize(app.Description, SummaryLimit)
	normalize(&app)
	return app, nil
}

func validateApp(app *App, where, stem string, categories map[string]bool) error {
	switch {
	case app.ID == "":
		return fmt.Errorf("%s: id is required", where)
	case app.ID != stem:
		return fmt.Errorf("%s: id %q disagrees with the file name %q", where, app.ID, stem)
	case app.Name == "":
		return fmt.Errorf("%s: name is required", where)
	case app.Tagline == "":
		return fmt.Errorf("%s: tagline is required", where)
	case app.Summary != "":
		return fmt.Errorf("%s: summary is derived from description and must not be authored", where)
	case app.Description == "":
		return fmt.Errorf("%s: description is required", where)
	case app.PrimaryCategory == "":
		return fmt.Errorf("%s: primary_category is required", where)
	case !categories[app.PrimaryCategory]:
		return fmt.Errorf("%s: primary_category %q is not a known category", where, app.PrimaryCategory)
	case !contains(app.Categories, app.PrimaryCategory):
		return fmt.Errorf("%s: primary_category %q is missing from categories", where, app.PrimaryCategory)
	case app.Protection != "" && !knownProtections[app.Protection]:
		return fmt.Errorf("%s: protection %q is not one of guarded, shareable", where, app.Protection)
	}

	for _, category := range app.Categories {
		if !categories[category] {
			return fmt.Errorf("%s: category %q is not a known category", where, category)
		}
	}
	if !knownIconModes[app.Icon.Mode] {
		return fmt.Errorf("%s: icon.mode %q is not one of mask, image", where, app.Icon.Mode)
	}
	if app.Icon.Mode == "mask" {
		if app.Icon.File == "" {
			return fmt.Errorf("%s: icon.file is required when icon.mode is mask", where)
		}
		// A consumer that builds its own icon assets cannot do so without
		// being told which glyph to cut, and would silently ship a blank
		// plate.
		if app.Icon.Mark == nil || app.Icon.Mark.Name == "" {
			return fmt.Errorf("%s: icon.mark is required when icon.mode is mask", where)
		}
		if !knownIconSets[app.Icon.Mark.Set] {
			return fmt.Errorf("%s: icon.mark.set %q is not one of simple-icons, lucide", where, app.Icon.Mark.Set)
		}
	}
	if app.Icon.Mode == "image" && app.Icon.Img == "" {
		return fmt.Errorf("%s: icon.img is required when icon.mode is image", where)
	}
	if app.Icon.Hue < 0 || app.Icon.Hue > 360 {
		return fmt.Errorf("%s: icon hue %d is outside 0-360", where, app.Icon.Hue)
	}
	for index, method := range app.Methods {
		if method.Name == "" {
			return fmt.Errorf("%s: method %d has no name", where, index)
		}
	}
	for index, limit := range app.Limits {
		if limit.Label == "" {
			return fmt.Errorf("%s: limit %d has no label", where, index)
		}
	}
	return nil
}

// normalize replaces every absent list with an empty one. A consumer decoding
// this document into a typed struct sees `[]`, not `null`, and can range over
// it without a nil check in a template.
func normalize(app *App) {
	app.Categories = nonNilStrings(app.Categories)
	app.Keywords = nonNilStrings(app.Keywords)
	app.Grants = nonNilStrings(app.Grants)
	app.Runtimes = nonNilStrings(app.Runtimes)
	if app.Methods == nil {
		app.Methods = []Method{}
	}
	if app.Changelog == nil {
		app.Changelog = []Release{}
	}
	for index := range app.Changelog {
		app.Changelog[index].Notes = nonNilStrings(app.Changelog[index].Notes)
	}
	if app.Limits == nil {
		app.Limits = []Limit{}
	}
	if app.Bundles == nil {
		app.Bundles = []Bundle{}
	}
	if app.Depends == nil {
		app.Depends = []Dependency{}
	}
	if demo := app.ProductDemo; demo != nil {
		demo.Examples = nonNilSteps(demo.Examples)
		demo.Gotchas = nonNilStrings(demo.Gotchas)
		demo.Next = nonNilStrings(demo.Next)
		if demo.Cost != nil && demo.Cost.Operations == nil {
			demo.Cost.Operations = []DemoCostOp{}
		}
	}
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func nonNilSteps(values []DemoStep) []DemoStep {
	if values == nil {
		return []DemoStep{}
	}
	return values
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
