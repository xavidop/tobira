package oracle

import (
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestQuoteFolded(t *testing.T) {
	for _, tc := range []struct {
		in   driver.Completion
		want string
	}{
		{driver.Completion{Label: "tobira_people", Kind: "collection"}, `"tobira_people"`},
		{driver.Completion{Label: "TOBIRA_PEOPLE", Kind: "collection"}, ""},
		{driver.Completion{Label: "Mixed", Kind: "field"}, `"Mixed"`},
		{driver.Completion{Label: "tobira", Kind: "namespace", Insert: "tobira."}, `"tobira".`},
		{driver.Completion{Label: "TOBIRA", Kind: "namespace", Insert: "TOBIRA."}, "TOBIRA."},
		{driver.Completion{Label: "select", Kind: "keyword"}, ""},
		{driver.Completion{Label: "a b", Kind: "field", Insert: `"a b"`}, `"a b"`},
	} {
		c := tc.in
		quoteFolded(&c)
		if c.Insert != tc.want {
			t.Errorf("quoteFolded(%+v).Insert = %q, want %q", tc.in, c.Insert, tc.want)
		}
	}
}
