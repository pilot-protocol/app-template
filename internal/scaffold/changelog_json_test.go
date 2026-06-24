package scaffold
import ("encoding/json";"strings";"testing")
func TestChangelogJSONLowercase(t *testing.T) {
	c := &Config{ID:"io.x.y", Namespace:"y", AppVersion:"0.1.0", Description:"d",
		Listing: Listing{Changelog: []ChangelogRel{{Version:"0.1.0", Notes:[]string{"first"}}}}}
	b, _ := json.Marshal(BuildMetadata(c))
	s := string(b)
	for _, k := range []string{`"version"`, `"notes"`} {
		if !strings.Contains(s, k) { t.Errorf("metadata changelog missing lowercase key %s", k) }
	}
	for _, k := range []string{`"Version"`, `"Notes"`, `"Date"`} {
		if strings.Contains(s, k) { t.Errorf("metadata changelog has Go-cased key %s", k) }
	}
}
