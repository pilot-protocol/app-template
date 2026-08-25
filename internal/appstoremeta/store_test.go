// SPDX-License-Identifier: AGPL-3.0-or-later

package appstoremeta

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"
)

// validIndex and validApp are the smallest documents that pass, so each test
// can corrupt exactly one thing and name the rule it is checking.
const validIndex = `{
  "schema_version": 1,
  "categories": [{"id":"data","name":"Data & Storage","blurb":"Databases.","hue":125}],
  "featured_order": ["io.pilot.postgres"]
}`

const validApp = `{
  "id": "io.pilot.postgres",
  "name": "PostgreSQL",
  "tagline": "Run and query PostgreSQL from an agent",
  "description": "# PostgreSQL\n\nThis app installs the **official** toolchain.",
  "categories": ["data"],
  "primary_category": "data",
  "protection": "guarded",
  "featured": true,
  "icon": {"mode":"mask","file":"/appicons/io.pilot.postgres.svg","mark":{"set":"simple-icons","name":"postgresql"},"color":"#4169E1","ink":false,"hue":125},
  "methods": [{"name":"postgres.query","summary":"Run SQL."}]
}`

func fsWith(index string, apps map[string]string) fstest.MapFS {
	files := fstest.MapFS{"index.json": &fstest.MapFile{Data: []byte(index)}}
	for name, body := range apps {
		files["apps/"+name+".json"] = &fstest.MapFile{Data: []byte(body)}
	}
	return files
}

func loadOK(t *testing.T, index string, apps map[string]string) *Document {
	t.Helper()
	document, err := Load(fsWith(index, apps))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return document
}

func TestLoadReadsEveryAppFile(t *testing.T) {
	second := strings.Replace(strings.Replace(validApp,
		`"io.pilot.postgres"`, `"io.pilot.duckdb"`, 1),
		`/appicons/io.pilot.postgres.svg`, `/appicons/io.pilot.duckdb.svg`, 1)

	document := loadOK(t, validIndex, map[string]string{"io.pilot.postgres": validApp, "io.pilot.duckdb": second})

	if len(document.Apps) != 2 {
		t.Fatalf("want 2 apps, got %d", len(document.Apps))
	}
	if document.SchemaVersion != SchemaVersion {
		t.Errorf("schema version: got %d want %d", document.SchemaVersion, SchemaVersion)
	}
	if len(document.Categories) != 1 || document.Categories[0].ID != "data" {
		t.Errorf("categories not loaded: %+v", document.Categories)
	}
}

func TestLoadOrdersAppsByIDForAStableDigest(t *testing.T) {
	// The ETag is a digest of the served bytes. Map iteration order would make
	// it change on every restart and defeat every consumer's cache.
	second := strings.Replace(validApp, `"io.pilot.postgres"`, `"io.pilot.aaa"`, 1)
	document := loadOK(t, `{"schema_version":1,"categories":[{"id":"data","name":"D","blurb":"b","hue":1}],"featured_order":[]}`,
		map[string]string{"io.pilot.postgres": validApp, "io.pilot.aaa": second})

	if document.Apps[0].ID != "io.pilot.aaa" || document.Apps[1].ID != "io.pilot.postgres" {
		t.Errorf("apps are not sorted by id: %s, %s", document.Apps[0].ID, document.Apps[1].ID)
	}
}

func TestLoadDerivesSummaryFromDescription(t *testing.T) {
	document := loadOK(t, validIndex, map[string]string{"io.pilot.postgres": validApp})

	app := document.Apps[0]
	if app.Summary != "This app installs the official toolchain." {
		t.Errorf("summary not derived: %q", app.Summary)
	}
	if !strings.HasPrefix(app.Description, "# PostgreSQL") {
		t.Errorf("authored markdown must survive alongside the summary: %q", app.Description)
	}
}

func TestLoadRejectsAnAuthoredSummary(t *testing.T) {
	// A hand-written summary is the drift this whole change removes: it would
	// silently outlive the description it is meant to condense.
	withSummary := strings.Replace(validApp, `"description":`, `"summary": "hand written", "description":`, 1)
	_, err := Load(fsWith(validIndex, map[string]string{"io.pilot.postgres": withSummary}))
	if err == nil || !strings.Contains(err.Error(), "summary") {
		t.Errorf("want a summary-is-derived error, got %v", err)
	}
}

func TestLoadNormalizesNilSlicesToEmpty(t *testing.T) {
	// A JSON null renders as a missing section on one consumer and a crash on
	// the other. Serving [] costs nothing and removes the question.
	document := loadOK(t, validIndex, map[string]string{"io.pilot.postgres": validApp})
	app := document.Apps[0]
	for name, value := range map[string]any{
		"keywords": app.Keywords, "changelog": app.Changelog, "grants": app.Grants,
		"limits": app.Limits, "bundles": app.Bundles, "depends": app.Depends, "runtimes": app.Runtimes,
	} {
		encoded, _ := json.Marshal(value)
		if string(encoded) == "null" {
			t.Errorf("%s encodes as null, want []", name)
		}
	}
}

func TestLoadRejectsInvalidDocuments(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		index   string
		app     string
		wantErr string
	}{
		{"no id", validIndex, strings.Replace(validApp, `"id": "io.pilot.postgres",`, ``, 1), "id"},
		{"no name", validIndex, strings.Replace(validApp, `"name": "PostgreSQL",`, ``, 1), "name"},
		{"no tagline", validIndex, strings.Replace(validApp, `"tagline": "Run and query PostgreSQL from an agent",`, ``, 1), "tagline"},
		{"filename disagrees with id", validIndex, strings.Replace(validApp, `"io.pilot.postgres"`, `"io.pilot.other"`, 1), "file name"},
		{"unknown primary category", validIndex, strings.Replace(validApp, `"primary_category": "data"`, `"primary_category": "nope"`, 1), "primary_category"},
		{"primary category missing from categories", validIndex, strings.Replace(validApp, `"categories": ["data"]`, `"categories": ["ai"]`, 1), "primary_category"},
		{"unknown protection", validIndex, strings.Replace(validApp, `"guarded"`, `"open"`, 1), "protection"},
		{"unknown icon mode", validIndex, strings.Replace(validApp, `"mode":"mask"`, `"mode":"sprite"`, 1), "icon.mode"},
		{"mask icon without a file", validIndex, strings.Replace(validApp, `"file":"/appicons/io.pilot.postgres.svg",`, ``, 1), "icon.file"},
		{"mask icon without a mark", validIndex, strings.Replace(validApp, `"mark":{"set":"simple-icons","name":"postgresql"},`, ``, 1), "icon.mark"},
		{"mark from an unknown icon set", validIndex, strings.Replace(validApp, `"set":"simple-icons"`, `"set":"heroicons"`, 1), "icon.mark.set"},
		{"hue out of range", validIndex, strings.Replace(validApp, `"hue":125}`, `"hue":900}`, 1), "hue"},
		{"method without a name", validIndex, strings.Replace(validApp, `"name":"postgres.query"`, `"name":""`, 1), "method"},
		{"featured order names an unknown app", strings.Replace(validIndex, `"io.pilot.postgres"`, `"io.pilot.ghost"`, 1), validApp, "featured_order"},
		{"category hue out of range", strings.Replace(validIndex, `"hue":125`, `"hue":-3`, 1), validApp, "hue"},
		{"duplicate category id", strings.Replace(validIndex, `"featured_order"`, `"x":1,"featured_order"`, 1), validApp, ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.wantErr == "" {
				t.Skip("placeholder row")
			}
			_, err := Load(fsWith(testCase.index, map[string]string{"io.pilot.postgres": testCase.app}))
			if err == nil {
				t.Fatalf("want an error mentioning %q, got none", testCase.wantErr)
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Errorf("error %q does not mention %q", err, testCase.wantErr)
			}
		})
	}
}

func TestLoadRejectsDuplicateAppIDs(t *testing.T) {
	// Two files, same id inside. The file name check catches it, but the
	// message must name the collision rather than the file.
	clash := strings.Replace(validApp, `"name": "PostgreSQL"`, `"name": "Clash"`, 1)
	_, err := Load(fstest.MapFS{
		"index.json":                  &fstest.MapFile{Data: []byte(validIndex)},
		"apps/io.pilot.postgres.json": &fstest.MapFile{Data: []byte(validApp)},
		"apps/io.pilot.postgres.JSON": &fstest.MapFile{Data: []byte(clash)},
	})
	if err == nil {
		t.Fatal("want an error for a duplicate id")
	}
}

func TestLoadRejectsMalformedJSON(t *testing.T) {
	_, err := Load(fsWith(validIndex, map[string]string{"io.pilot.postgres": "{not json"}))
	if err == nil || !strings.Contains(err.Error(), "io.pilot.postgres") {
		t.Errorf("a parse error must name the offending file, got %v", err)
	}
}

func TestLoadRejectsAnEmptyCatalogue(t *testing.T) {
	// An empty document would push both consumers onto their fallbacks with no
	// signal that the data directory was simply missing.
	if _, err := Load(fsWith(validIndex, nil)); err == nil {
		t.Error("want an error for a document with no apps")
	}
}

func TestDocumentLookupByID(t *testing.T) {
	document := loadOK(t, validIndex, map[string]string{"io.pilot.postgres": validApp})

	if app, ok := document.App("io.pilot.postgres"); !ok || app.Name != "PostgreSQL" {
		t.Errorf("App() missed a present app: %+v %v", app, ok)
	}
	if _, ok := document.App("io.pilot.ghost"); ok {
		t.Error("App() found an absent app")
	}
}

func TestLoadHonoursTheCuratedOrder(t *testing.T) {
	// The store renders apps in array order, so this is what keeps a shelf
	// looking the same on both surfaces.
	second := strings.Replace(validApp, `"io.pilot.postgres"`, `"io.pilot.aaa"`, 1)
	index := strings.Replace(validIndex, `"featured_order": ["io.pilot.postgres"]`,
		`"featured_order": ["io.pilot.postgres"], "app_order": ["io.pilot.postgres", "io.pilot.aaa"]`, 1)

	document := loadOK(t, index, map[string]string{"io.pilot.postgres": validApp, "io.pilot.aaa": second})

	if document.Apps[0].ID != "io.pilot.postgres" || document.Apps[1].ID != "io.pilot.aaa" {
		t.Errorf("curated order ignored: %s, %s", document.Apps[0].ID, document.Apps[1].ID)
	}
}

func TestLoadPlacesUnlistedAppsAfterTheCuratedOnes(t *testing.T) {
	// An app must never become invisible because someone forgot to list it.
	second := strings.Replace(validApp, `"io.pilot.postgres"`, `"io.pilot.aaa"`, 1)
	index := strings.Replace(validIndex, `"featured_order": ["io.pilot.postgres"]`,
		`"featured_order": ["io.pilot.postgres"], "app_order": ["io.pilot.postgres"]`, 1)

	document := loadOK(t, index, map[string]string{"io.pilot.postgres": validApp, "io.pilot.aaa": second})

	if len(document.Apps) != 2 {
		t.Fatalf("an unlisted app was dropped: %d apps", len(document.Apps))
	}
	if document.Apps[0].ID != "io.pilot.postgres" || document.Apps[1].ID != "io.pilot.aaa" {
		t.Errorf("unlisted app was not placed last: %s, %s", document.Apps[0].ID, document.Apps[1].ID)
	}
}

func TestLoadRejectsAnAppOrderNamingAnAbsentApp(t *testing.T) {
	index := strings.Replace(validIndex, `"featured_order": ["io.pilot.postgres"]`,
		`"featured_order": ["io.pilot.postgres"], "app_order": ["io.pilot.ghost"]`, 1)

	_, err := Load(fsWith(index, map[string]string{"io.pilot.postgres": validApp}))
	if err == nil || !strings.Contains(err.Error(), "app_order") {
		t.Errorf("want an app_order error, got %v", err)
	}
}
