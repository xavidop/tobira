package github

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func browse(t *testing.T, c *conn, ref driver.CollectionRef, opts driver.BrowseOptions) ([]driver.Column, []driver.Row, driver.Stats) {
	t.Helper()
	rs, err := c.Browse(context.Background(), ref, opts)
	if err != nil {
		t.Fatalf("browse %+v: %v", opts, err)
	}
	rows, stats, err := driver.Collect(context.Background(), rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	return rs.Columns(), rows, stats
}

// Browse pages through the issues with a cursor: full pages carry a
// NextCursor and Truncated, the last page neither. Pull requests, which
// the issues endpoint lists too, are dropped.
func TestBrowsePagesThroughIssues(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	ref := driver.CollectionRef{Namespace: "octo/widgets", Name: "issues"}
	cols, first, stats := browse(t, c, ref, driver.BrowseOptions{})
	if len(first) != 100 || !stats.Truncated || stats.NextCursor == "" {
		t.Fatalf("first page: %d rows, truncated %v, cursor %q", len(first), stats.Truncated, stats.NextCursor)
	}
	if len(cols) != len(issueColumns) || cols[0].Name != "number" || !cols[0].Key || cols[0].Source == nil || cols[0].Source.Name != "issues" {
		t.Errorf("columns = %+v", cols)
	}
	seen := map[int64]bool{}
	for _, row := range first {
		n, _ := row[0].AsInt()
		if n > 250 {
			t.Errorf("a pull request (#%d) is listed among the issues", n)
		}
		seen[n] = true
	}
	_, second, stats := browse(t, c, ref, driver.BrowseOptions{Cursor: stats.NextCursor})
	if len(second) != 100 || !stats.Truncated || stats.NextCursor == "" {
		t.Fatalf("second page: %d rows, truncated %v, cursor %q", len(second), stats.Truncated, stats.NextCursor)
	}
	_, third, stats := browse(t, c, ref, driver.BrowseOptions{Cursor: stats.NextCursor})
	if len(third) != 50 || stats.Truncated || stats.NextCursor != "" {
		t.Fatalf("last page: %d rows, truncated %v, cursor %q", len(third), stats.Truncated, stats.NextCursor)
	}
	for _, page := range [][]driver.Row{second, third} {
		for _, row := range page {
			n, _ := row[0].AsInt()
			if seen[n] {
				t.Errorf("issue #%d appears on two pages", n)
			}
			seen[n] = true
		}
	}
	if len(seen) != 250 {
		t.Errorf("%d distinct issues over three pages, want 250", len(seen))
	}
}

// Conditions the endpoint can take are sent as its parameters and the rows
// come back filtered; a smaller page size is the endpoint's per_page.
func TestBrowsePushesFiltersToTheEndpoint(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.resetLog()
	ref := driver.CollectionRef{Namespace: "octo/widgets", Name: "pull_requests"}
	opts := driver.BrowseOptions{Limit: 5, Filter: driver.Filter{Conditions: []driver.Condition{
		{Field: "state", Op: driver.OpEq, Value: driver.String("open")},
		{Field: "base", Op: driver.OpEq, Value: driver.String("main")},
	}}}
	cols, rows, stats := browse(t, c, ref, opts)
	if len(rows) != 5 || !stats.Truncated {
		t.Fatalf("filtered page: %d rows, truncated %v", len(rows), stats.Truncated)
	}
	reqs := f.requestsTo("/repos/octo/widgets/pulls")
	if len(reqs) != 1 || reqs[0].Query.Get("state") != "open" || reqs[0].Query.Get("base") != "main" || reqs[0].Query.Get("per_page") != "5" {
		t.Errorf("requests = %+v", reqs)
	}
	for _, row := range rows {
		state, _ := cell(t, cols, row, "state").AsText()
		base, _ := cell(t, cols, row, "base").AsText()
		if state != "open" || base != "main" {
			t.Errorf("row state=%s base=%s does not match the filter", state, base)
		}
	}
	// Issues drop pull requests after the read, so their pages are always
	// full ones, whatever the page size; the filters still go through.
	f.resetLog()
	issues := driver.CollectionRef{Namespace: "octo/widgets", Name: "issues"}
	cols, rows, _ = browse(t, c, issues, driver.BrowseOptions{Limit: 20, Filter: driver.Filter{Conditions: []driver.Condition{
		{Field: "state", Op: driver.OpEq, Value: driver.String("open")},
		{Field: "labels", Op: driver.OpContains, Value: driver.String("bug")},
	}}})
	reqs = f.requestsTo("/repos/octo/widgets/issues")
	if len(rows) != 20 || len(reqs) != 1 || reqs[0].Query.Get("state") != "open" || reqs[0].Query.Get("labels") != "bug" || reqs[0].Query.Get("per_page") != "100" {
		t.Errorf("%d rows, requests = %+v", len(rows), reqs)
	}
	for _, row := range rows {
		labels, _ := cell(t, cols, row, "labels").AsList()
		hasBug := false
		for _, l := range labels {
			if s, _ := l.AsText(); s == "bug" {
				hasBug = true
			}
		}
		if state, _ := cell(t, cols, row, "state").AsText(); state != "open" || !hasBug {
			t.Errorf("row state=%s labels=%v does not match the filter", state, labels)
		}
	}
}

// A cursor from a browse with other parameters is refused.
func TestBrowseRefusesAForeignCursor(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	ref := driver.CollectionRef{Namespace: "octo/widgets", Name: "labels"}
	_, _, stats := browse(t, c, ref, driver.BrowseOptions{Limit: 3})
	if stats.NextCursor == "" {
		t.Fatal("no cursor on a partial page")
	}
	_, err := c.Browse(context.Background(), ref, driver.BrowseOptions{Limit: 4, Cursor: stats.NextCursor})
	wantKind(t, "a cursor with another page size", err, driver.ErrSyntax)
	_, err = c.Browse(context.Background(), ref, driver.BrowseOptions{Limit: 3, Cursor: "junk"})
	wantKind(t, "a junk cursor", err, driver.ErrSyntax)
}
