//go:build integration

package postgres

import (
	"slices"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

func completeAt(t *testing.T, c *conn, text string) []driver.Completion {
	t.Helper()
	i := strings.IndexByte(text, '|')
	got, err := c.Complete(ctxT(t), driver.CompletionRequest{Text: text[:i] + text[i+1:], Cursor: i})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func labelsOf(cs []driver.Completion) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Label
	}
	return out
}

func TestCompleteIntegration(t *testing.T) {
	for _, ro := range []bool{false, true} {
		c := openPG(t, ro)
		if got := labelsOf(completeAt(t, c, "select * from tobira_p|")); !slices.Equal(got, []string{"tobira_people", "tobira_people_mv", "tobira_people_view"}) {
			t.Errorf("ro=%v collections = %v", ro, got)
		}
		if got := labelsOf(completeAt(t, c, "select p.na| from tobira_people p")); !slices.Equal(got, []string{"name"}) {
			t.Errorf("ro=%v alias fields = %v", ro, got)
		}
		if got := labelsOf(completeAt(t, c, "select * from public.tobira_p|")); !slices.Equal(got, []string{"tobira_people", "tobira_people_mv", "tobira_people_view"}) {
			t.Errorf("ro=%v schema-qualified = %v", ro, got)
		}
		if got := labelsOf(completeAt(t, c, "select * from |")); !slices.Contains(got, "public") || !slices.Contains(got, "tobira_people") {
			t.Errorf("ro=%v from = %v", ro, got)
		}
		if got := completeAt(t, c, "select $$ from tobira_p|$$"); len(got) != 0 {
			t.Errorf("ro=%v inside dollar quote = %v", ro, got)
		}
	}
}

func TestCompleteQuotesMixedCase(t *testing.T) {
	c := openPG(t, false)
	mustExec(t, c, `CREATE TABLE "Tobira_Mixed" ("Id" int, "order" int, plain int)`)
	t.Cleanup(func() { mustExec(t, c, `DROP TABLE IF EXISTS "Tobira_Mixed"`) })
	c.Invalidate()
	got := completeAt(t, c, "select * from tobira_m|")
	if len(got) != 1 || got[0].Label != "Tobira_Mixed" || got[0].Insert != `"Tobira_Mixed"` {
		t.Fatalf("collection = %+v", got)
	}
	byLabel := map[string]string{}
	for _, f := range completeAt(t, c, `select m.| from "Tobira_Mixed" m`) {
		byLabel[f.Label] = f.Insert
	}
	want := map[string]string{"Id": `"Id"`, "order": `"order"`, "plain": ""}
	for k, v := range want {
		if got, ok := byLabel[k]; !ok || got != v {
			t.Errorf("field %q Insert = %q (present %v), want %q", k, got, ok, v)
		}
	}
}

func TestCompleteCacheAndInvalidate(t *testing.T) {
	c := openPG(t, false)
	mustExec(t, c, "DROP TABLE IF EXISTS tobira_cachetest")
	t.Cleanup(func() { mustExec(t, c, "DROP TABLE IF EXISTS tobira_cachetest") })
	if got := completeAt(t, c, "select * from tobira_cache|"); len(got) != 0 {
		t.Fatalf("before create = %v", labelsOf(got))
	}
	mustExec(t, c, "CREATE TABLE tobira_cachetest (a int)")
	if got := completeAt(t, c, "select * from tobira_cache|"); len(got) != 0 {
		t.Fatalf("catalog must be cached for 60 s, got %v", labelsOf(got))
	}
	var inv sqlcomplete.Invalidator = c
	inv.Invalidate()
	if got := labelsOf(completeAt(t, c, "select * from tobira_cache|")); !slices.Equal(got, []string{"tobira_cachetest"}) {
		t.Fatalf("after Invalidate = %v", got)
	}
}

func TestCompleteClosed(t *testing.T) {
	c, err := New().Open(ctxT(t), driver.ConnConfig{DSN: testDSN})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if _, err := c.(driver.Completer).Complete(ctxT(t), driver.CompletionRequest{Text: "select ", Cursor: 7}); err == nil {
		t.Fatal("closed conn must fail")
	}
}
