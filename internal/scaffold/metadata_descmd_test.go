package scaffold

import "testing"

func TestDescriptionMD_PrefersLong(t *testing.T) {
	long := "Long markdown\n- bullet one\n- bullet two"
	c := &Config{ID: "io.x.y", Namespace: "y", Description: "short one-liner",
		Listing: Listing{DisplayName: "Y", AppDescription: long}}
	if got := BuildMetadata(c).DescriptionMD; got != long {
		t.Errorf("description_md = %q, want the long app_description", got)
	}
	c.Listing.AppDescription = ""
	if got := BuildMetadata(c).DescriptionMD; got != "short one-liner" {
		t.Errorf("with no app_description, description_md = %q, want the short", got)
	}
}
