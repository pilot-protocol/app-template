package broker

import (
	"encoding/json"
	"strings"
)

// extractCost pulls a numeric cost (in cents) out of a partner JSON response.
// field is a dot-path into the JSON object, e.g. "cost_cents" or
// "usage.cost_cents". Empty field means "no cost metering for this app".
// Returns 0 when the path is absent or not a number.
func extractCost(body []byte, field string) float64 {
	if field == "" {
		return 0
	}
	var doc any
	if json.Unmarshal(body, &doc) != nil {
		return 0
	}
	cur := doc
	for _, seg := range strings.Split(field, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return 0
		}
		cur, ok = m[seg]
		if !ok {
			return 0
		}
	}
	if n, ok := cur.(float64); ok {
		return n
	}
	return 0
}
