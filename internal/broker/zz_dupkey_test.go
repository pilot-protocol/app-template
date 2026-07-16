package broker

import "testing"

// TestHasDuplicateKeys guards the parser-differential defense. Go's decoder keeps
// the LAST duplicate key; a partner parser keeping the FIRST would act on a value
// the broker never authorized. These cases pin that door shut.
func TestHasDuplicateKeys(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"clean object", `{"agent_id":"a1","to":"+1555"}`, false},
		{"repeated key, victim first", `{"agent_id":"victim","agent_id":"mine"}`, true},
		{"dup key reversed", `{"agent_id":"mine","agent_id":"victim"}`, true},
		{"dup nested", `{"m":{"agent_id":"victim","agent_id":"mine"}}`, true},
		{"dup in array element", `{"b":[{"agent_id":"v","agent_id":"m"}]}`, true},
		{"same key different objects is fine", `{"a":{"id":"1"},"b":{"id":"2"}}`, false},
		{"repeated key across array elements is fine", `[{"id":"1"},{"id":"2"}]`, false},
		{"value equal to a key name is fine", `{"a":"b","b":"c"}`, false},
		{"string value matching sibling key", `{"agent_id":"agent_id","x":"y"}`, false},
		{"numbers and nulls", `{"a":1,"b":null,"c":true,"d":[1,2]}`, false},
		{"deep nesting clean", `{"a":{"b":{"c":{"d":{"id":"x"}}}}}`, false},
		{"malformed → treated as ambiguous", `{"a":`, true},
		{"empty object", `{}`, false},
		{"array of scalars", `["a","a"]`, false},
		{"dup after nested object closes", `{"n":{"x":1},"k":1,"k":2}`, true},
	}
	for _, c := range cases {
		if got := hasDuplicateKeys([]byte(c.body)); got != c.want {
			t.Errorf("%s: hasDuplicateKeys(%s) = %v, want %v", c.name, c.body, got, c.want)
		}
	}
}
