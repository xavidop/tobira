package dynamodb

import (
	"slices"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func labels(cs []driver.Completion) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Label)
	}
	return out
}

func TestCompletionsFor(t *testing.T) {
	tables := []string{"tobira_people", "orders", "my-table"}
	attrs := func(table string) []string {
		if table == "tobira_people" {
			return []string{"id", "name", "nickname"}
		}
		return nil
	}
	for _, tc := range []struct {
		text      string
		cursor    int
		want      []string
		notWant   []string
		insertFor map[string]string
	}{
		{text: "SELECT * FROM tobira_", want: []string{"tobira_people"}, notWant: []string{"SELECT"}},
		{text: `SELECT * FROM "tob`, want: []string{"tobira_people"}, insertFor: map[string]string{"tobira_people": "tobira_people"}},
		{text: "SELECT * FROM my", want: []string{"my-table"}, insertFor: map[string]string{"my-table": `"my-table"`}},
		{text: "sel", want: []string{"SELECT"}},
		{text: "SELECT * FROM tobira_people WHERE n", want: []string{"name", "nickname", "NOT"}},
		{text: "SELECT * FROM tobira_people WHERE begin", want: []string{"begins_with"}},
		{text: "SELECT n FROM tobira_people", cursor: len("SELECT n"), want: []string{"name", "nickname"}},
		{text: "UPDATE o", want: []string{"orders"}},
	} {
		cur := tc.cursor
		if cur == 0 {
			cur = len(tc.text)
		}
		got := completionsFor(driver.CompletionRequest{Text: tc.text, Cursor: cur}, tables, attrs)
		ls := labels(got)
		for _, w := range tc.want {
			if !slices.Contains(ls, w) {
				t.Errorf("%q: missing %q in %v", tc.text, w, ls)
			}
		}
		for _, w := range tc.notWant {
			if slices.Contains(ls, w) {
				t.Errorf("%q: unexpected %q in %v", tc.text, w, ls)
			}
		}
		for label, ins := range tc.insertFor {
			i := slices.IndexFunc(got, func(c driver.Completion) bool { return c.Label == label })
			if i < 0 {
				continue
			}
			insert := got[i].Insert
			if insert == "" {
				insert = got[i].Label
			}
			if insert != ins {
				t.Errorf("%q: %s inserts %q, want %q", tc.text, label, insert, ins)
			}
		}
	}
	if got := completionsFor(driver.CompletionRequest{Text: "x", Cursor: 99}, tables, attrs); got != nil {
		t.Errorf("a cursor past the text must give nothing, got %v", labels(got))
	}
}
