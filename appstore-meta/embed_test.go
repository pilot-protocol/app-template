// SPDX-License-Identifier: AGPL-3.0-or-later

package appstoremetadata_test

import (
	"io/fs"
	"testing"

	appstoremetadata "github.com/pilot-protocol/app-template/appstore-meta"
	"github.com/pilot-protocol/app-template/internal/appstoremeta"
)

// data is the embedded directory rooted at data/, which is the layout Load
// expects.
func data(t *testing.T) fs.FS {
	t.Helper()
	sub, err := fs.Sub(appstoremetadata.Files, "data")
	if err != nil {
		t.Fatalf("sub: %v", err)
	}
	return sub
}

// TestShippedDataLoads is the gate on the data itself: every rule in the
// loader runs against every real app file on every CI run, so a bad edit fails
// here rather than on a live app page.
func TestShippedDataLoads(t *testing.T) {
	document, err := appstoremeta.Load(data(t))
	if err != nil {
		t.Fatalf("shipped data does not load: %v", err)
	}
	if len(document.Apps) == 0 {
		t.Fatal("shipped data has no apps")
	}
	t.Logf("loaded %d apps across %d categories", len(document.Apps), len(document.Categories))

	for _, app := range document.Apps {
		if app.Summary == "" {
			t.Errorf("%s: derived summary is empty", app.ID)
		}
		if len([]rune(app.Summary)) > appstoremeta.SummaryLimit {
			t.Errorf("%s: summary is %d runes, over the %d limit", app.ID, len([]rune(app.Summary)), appstoremeta.SummaryLimit)
		}
	}
}

// TestEveryFeaturedAppIsFlaggedBothWays keeps the carousel and the per-app
// flag from disagreeing: the console picks the carousel from the flag and the
// site from the order, so a mismatch shows a different hero on each surface.
func TestEveryFeaturedAppIsFlaggedBothWays(t *testing.T) {
	document, err := appstoremeta.Load(data(t))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, id := range document.FeaturedOrder {
		app, ok := document.App(id)
		if !ok {
			t.Fatalf("featured_order names %q, which is absent", id)
		}
		if !app.Featured {
			t.Errorf("%s is in featured_order but not marked featured", id)
		}
	}
	for _, app := range document.Apps {
		if !app.Featured {
			continue
		}
		found := false
		for _, id := range document.FeaturedOrder {
			found = found || id == app.ID
		}
		if !found {
			t.Errorf("%s is marked featured but missing from featured_order", app.ID)
		}
	}
}
