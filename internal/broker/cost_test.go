package broker

import "testing"

func TestExtractCost(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		field string
		want  float64
	}{
		{"top level", `{"cost_cents":5}`, "cost_cents", 5},
		{"nested", `{"usage":{"cost_cents":12.5}}`, "usage.cost_cents", 12.5},
		{"missing", `{"ok":true}`, "cost_cents", 0},
		{"empty field disables", `{"cost_cents":5}`, "", 0},
		{"non-number", `{"cost_cents":"oops"}`, "cost_cents", 0},
		{"bad json", `not json`, "cost_cents", 0},
		{"path through non-object", `{"a":5}`, "a.b", 0},
	}
	for _, c := range cases {
		if got := extractCost([]byte(c.body), c.field); got != c.want {
			t.Errorf("%s: extractCost(%q,%q) = %v, want %v", c.name, c.body, c.field, got, c.want)
		}
	}
}
