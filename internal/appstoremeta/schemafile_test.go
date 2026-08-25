// SPDX-License-Identifier: AGPL-3.0-or-later

package appstoremeta

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// jsonFieldNames lists the wire names a struct encodes, so a schema file can
// be checked against the type that actually produces the bytes.
func jsonFieldNames(value any) []string {
	structType := reflect.TypeOf(value)
	names := make([]string, 0, structType.NumField())
	for index := 0; index < structType.NumField(); index++ {
		tag := structType.Field(index).Tag.Get("json")
		name := strings.SplitN(tag, ",", 2)[0]
		if name == "" || name == "-" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func schemaProperties(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(body, &schema); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	names := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestPublishedSchemaMatchesTheServedType is the guard that keeps the
// documented contract honest. The schema file is what an outside author reads
// and what the site's generator is written against, so a field added to the Go
// type without a schema entry would ship undocumented — and a schema entry
// with no field would document something that never appears.
func TestPublishedSchemaMatchesTheServedType(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		path   string
		fields []string
	}{
		{"app", "../../appstore-meta/schema/app.schema.json", jsonFieldNames(App{})},
		{"index", "../../appstore-meta/schema/index.schema.json", []string{"schema_version", "source", "categories", "featured_order"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			want := append([]string(nil), testCase.fields...)
			sort.Strings(want)
			got := schemaProperties(t, testCase.path)

			for _, field := range want {
				if !containsString(got, field) {
					t.Errorf("%q is served but missing from %s", field, testCase.path)
				}
			}
			for _, field := range got {
				if !containsString(want, field) {
					t.Errorf("%q is in %s but is never served", field, testCase.path)
				}
			}
		})
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
