package snowflake

import (
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestSQLName(t *testing.T) {
	for in, want := range map[string]string{
		"TOBIRA_PEOPLE": "TOBIRA_PEOPLE",
		"A$B1":          "A$B1",
		"tobira_people": `"tobira_people"`,
		"Mixed":         `"Mixed"`,
		"1ABC":          `"1ABC"`,
		"TABLE":         `"TABLE"`,
		`A"B`:           `"A""B"`,
		"":              `""`,
	} {
		if got := sqlName(in); got != want {
			t.Errorf("sqlName(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestSFInsert(t *testing.T) {
	for _, c := range []struct {
		in   driver.Completion
		want string
	}{
		{driver.Completion{Label: "tobira_people", Kind: "collection"}, `"tobira_people"`},
		{driver.Completion{Label: "PEOPLE", Kind: "collection"}, ""},
		{driver.Completion{Label: "tobira_people", Kind: "collection", Insert: `"tobira_people"`}, `"tobira_people"`},
		{driver.Completion{Label: "id", Kind: "field"}, `"id"`},
		{driver.Completion{Label: "SALES.PUBLIC", Kind: "namespace", Insert: `"SALES.PUBLIC".`}, "SALES.PUBLIC."},
		{driver.Completion{Label: `"my.db".s`, Kind: "namespace", Insert: `"""my.db"".s".`}, `"my.db"."s".`},
		{driver.Completion{Label: "SELECT", Kind: "keyword"}, ""},
	} {
		if got := sfInsert(c.in); got != c.want {
			t.Errorf("sfInsert(%+v) = %q, want %q", c.in, got, c.want)
		}
	}
}
