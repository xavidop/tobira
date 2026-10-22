package vertica

import (
	"testing"

	"github.com/xavidop/tobira/driver"
)

// Vertica folds unquoted names to lower case for display and matches them
// case-insensitively, so only names that are not plain identifiers, or are
// reserved, need quotes.
func TestSQLName(t *testing.T) {
	for in, want := range map[string]string{
		"tobira_people": "tobira_people", "People": "People", "a$b": "a$b",
		"select": `"select"`, "my table": `"my table"`, "1st": `"1st"`, "": `""`, `a"b`: `"a""b"`,
	} {
		if got := sqlName(in); got != want {
			t.Errorf("sqlName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVerticaInsert(t *testing.T) {
	for _, tt := range []struct {
		c    driver.Completion
		want string
	}{
		{driver.Completion{Kind: "keyword", Label: "SELECT", Insert: "SELECT"}, "SELECT"},
		{driver.Completion{Kind: "namespace", Label: "Mixed Space", Insert: "Mixed Space"}, `"Mixed Space".`},
		{driver.Completion{Kind: "namespace", Label: "public", Insert: "public"}, "public."},
		{driver.Completion{Kind: "collection", Label: "order", Insert: "order"}, `"order"`},
		{driver.Completion{Kind: "collection", Label: "tobira_people", Insert: "tobira_people"}, "tobira_people"},
		{driver.Completion{Kind: "field", Label: "x", Insert: `"x"`}, `"x"`},
	} {
		if got := verticaInsert(tt.c); got != tt.want {
			t.Errorf("verticaInsert(%+v) = %q, want %q", tt.c, got, tt.want)
		}
	}
}
