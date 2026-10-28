package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

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

var issuesRef = driver.CollectionRef{Namespace: "octo/widgets", Name: "issues"}

// seededItems copies a repository's collection out of the fake, so a test
// can work out what a filter should match on its own.
func seededItems(f *fakeGitHub, owner, repo, kind string) []object {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.findRepo(owner, repo).collection(kind))
}

// pagesRequested lists the page numbers of requests, in order.
func pagesRequested(reqs []fakeRequest) []int {
	out := make([]int, len(reqs))
	for i, r := range reqs {
		out[i] = atoiDefault(r.Query.Get("page"), 0)
	}
	return out
}

// rowNumbers reads the key column of every row as an integer.
func rowNumbers(t *testing.T, cols []driver.Column, rows []driver.Row, name string) []int64 {
	t.Helper()
	out := make([]int64, len(rows))
	for i, row := range rows {
		out[i], _ = cell(t, cols, row, name).AsInt()
	}
	return out
}

// syntheticIssuePages answers the issues listing of octo/widgets with the
// given number of pages of 100 open issues, the first pullRequests items
// of each page being pull requests, so a Browse can scan far past the
// seeded listing.
func syntheticIssuePages(pages, pullRequests int) func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
	return func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path != "/repos/octo/widgets/issues" || r.Method != http.MethodGet {
			return false
		}
		page := atoiDefault(r.URL.Query().Get("page"), 1)
		items := make([]object, 0, maxPerPage)
		for i := 0; i < maxPerPage; i++ {
			n := (page-1)*maxPerPage + i + 1
			it := object{"number": n, "title": fmt.Sprintf("Synthetic issue %d", n), "state": "open", "user": userObj("octo"),
				"labels": []object{}, "assignees": []object{}, "comments": 0, "created_at": at(0), "updated_at": at(0)}
			if i < pullRequests {
				it["pull_request"] = object{}
			}
			items = append(items, it)
		}
		if link := linkHeader(r, page, maxPerPage, pages*maxPerPage); link != "" {
			w.Header().Set("Link", link)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		body, _ := json.Marshal(items)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return true
	}
}

// A filter mixing conditions the issues endpoint takes with ones it does
// not: the first kind travel as query parameters (a second label, a title
// match, a comment count and a null check stay in tobira), and the rows
// are exactly the issues satisfying every condition.
func TestBrowseAppliesTheRestOfTheFilterInTobira(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.resetLog()
	since := seedBase.Add(time.Duration(100*60+30) * time.Minute)
	conds := []driver.Condition{
		{Field: "title", Op: driver.OpContains, Value: driver.String("Issue 2")},
		{Field: "state", Op: driver.OpEq, Value: driver.String("open")},
		{Field: "comments", Op: driver.OpGt, Value: driver.Int(3)},
		{Field: "labels", Op: driver.OpContains, Value: driver.String("enhancement")},
		{Field: "labels", Op: driver.OpContains, Value: driver.String("documentation")},
		{Field: "assignees", Op: driver.OpIsNull},
		{Field: "user", Op: driver.OpEq, Value: driver.String("hubot")},
		{Field: "updated_at", Op: driver.OpGe, Value: driver.Time(since)},
		{Field: "closed_at", Op: driver.OpIsNull},
	}
	want := map[int64]bool{}
	for _, is := range seededItems(f, "octo", "widgets", "issues") {
		if _, pr := is["pull_request"]; pr {
			continue
		}
		labels := memberNames(is, "labels", "name")
		if strings.Contains(str(is, "title"), "Issue 2") && str(is, "state") == "open" && num(is, "comments") > 3 &&
			slices.Contains(labels, "enhancement") && slices.Contains(labels, "documentation") &&
			len(memberNames(is, "assignees", "login")) == 0 && str(sub(is, "user"), "login") == "hubot" &&
			!tm(is, "updated_at").Before(since) && is["closed_at"] == nil {
			want[num(is, "number")] = true
		}
	}
	if len(want) == 0 {
		t.Fatal("the seed has no issue matching the filter: the test proves nothing")
	}
	cols, rows, stats := browse(t, c, issuesRef, driver.BrowseOptions{Filter: driver.Filter{Conditions: conds}})
	got := map[int64]bool{}
	for _, n := range rowNumbers(t, cols, rows, "number") {
		got[n] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
	if stats.Truncated || stats.NextCursor != "" || len(stats.Warnings) != 0 {
		t.Errorf("stats = %+v, want a complete page", stats)
	}
	reqs := f.requestsTo("/repos/octo/widgets/issues")
	if len(reqs) == 0 {
		t.Fatal("no request to the issues endpoint")
	}
	for _, r := range reqs {
		q := r.Query
		if q.Get("state") != "open" || q.Get("assignee") != "none" || q.Get("creator") != "hubot" || q.Get("since") != "2024-01-05T04:30:00Z" || q.Get("labels") != "enhancement" {
			t.Errorf("pushed parameters = %v", q)
		}
		for _, k := range []string{"title", "comments", "closed_at"} {
			if q.Has(k) {
				t.Errorf("%q was sent as a parameter: %v", k, q)
			}
		}
	}
}

// A filter matching nothing reads the whole seeded listing in three
// requests and ends without a cursor. Over a longer listing the page stops
// after 1000 items with a warning and a cursor, and the next Browse goes on
// from the page after the stop.
func TestBrowseStopsScanningAfterAThousandItems(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	nothing := driver.BrowseOptions{Limit: 100, Filter: driver.Filter{Conditions: []driver.Condition{{Field: "title", Op: driver.OpEq, Value: driver.String("nothing")}}}}
	f.resetLog()
	_, rows, stats := browse(t, c, issuesRef, nothing)
	if pages := pagesRequested(f.requestsTo("/repos/octo/widgets/issues")); len(rows) != 0 || stats.Truncated || stats.NextCursor != "" || len(stats.Warnings) != 0 || !reflect.DeepEqual(pages, []int{1, 2, 3}) {
		t.Errorf("seeded listing: %d rows, stats %+v, pages %v", len(rows), stats, pages)
	}

	f.setHook(syntheticIssuePages(12, 0))
	f.resetLog()
	_, rows, stats = browse(t, c, issuesRef, nothing)
	pages := pagesRequested(f.requestsTo("/repos/octo/widgets/issues"))
	if len(rows) != 0 || !stats.Truncated || stats.NextCursor == "" {
		t.Fatalf("long listing: %d rows, stats %+v", len(rows), stats)
	}
	if len(stats.Warnings) != 1 || !strings.Contains(stats.Warnings[0], "looked at 1000 items and stopped early") {
		t.Errorf("warnings = %v", stats.Warnings)
	}
	if !reflect.DeepEqual(pages, []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}) {
		t.Errorf("pages read = %v, want 1 to 10", pages)
	}
	f.resetLog()
	next := nothing
	next.Cursor = stats.NextCursor
	_, rows, stats = browse(t, c, issuesRef, next)
	pages = pagesRequested(f.requestsTo("/repos/octo/widgets/issues"))
	if len(rows) != 0 || stats.Truncated || stats.NextCursor != "" || len(stats.Warnings) != 0 || !reflect.DeepEqual(pages, []int{11, 12}) {
		t.Errorf("continued page: %d rows, stats %+v, pages %v", len(rows), stats, pages)
	}
}

// When pull requests thin out the pages, the request cap ends the page
// before the item cap; the warning says so and the cursor resumes after
// the last page read.
func TestBrowseStopsAfterTenRequests(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.setHook(syntheticIssuePages(12, 10))
	f.resetLog()
	opts := driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "comments", Op: driver.OpGt, Value: driver.Int(0)}}}}
	_, rows, stats := browse(t, c, issuesRef, opts)
	if len(rows) != 0 || !stats.Truncated || stats.NextCursor == "" || len(f.log()) != 10 {
		t.Fatalf("%d rows, stats %+v, %d requests", len(rows), stats, len(f.log()))
	}
	if len(stats.Warnings) != 1 || !strings.Contains(stats.Warnings[0], "stopped after 10 requests") {
		t.Errorf("warnings = %v", stats.Warnings)
	}
	f.resetLog()
	opts.Cursor = stats.NextCursor
	_, _, stats = browse(t, c, issuesRef, opts)
	if pages := pagesRequested(f.log()); !reflect.DeepEqual(pages, []int{11, 12}) || stats.Truncated || stats.NextCursor != "" {
		t.Errorf("continued page: pages %v, stats %+v", pages, stats)
	}
}

// A page size over 1000 is cut with a warning; 0 means 100 rows.
func TestBrowsePageSizeBounds(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.resetLog()
	_, rows, stats := browse(t, c, issuesRef, driver.BrowseOptions{Limit: 1001})
	if len(rows) != 250 || stats.Truncated || len(f.log()) != 3 {
		t.Errorf("limit 1001: %d rows, truncated %v, %d requests", len(rows), stats.Truncated, len(f.log()))
	}
	if len(stats.Warnings) != 1 || stats.Warnings[0] != "the page size was cut from 1001 to 1000 rows" {
		t.Errorf("warnings = %v", stats.Warnings)
	}
	_, rows, stats = browse(t, c, issuesRef, driver.BrowseOptions{Limit: 0})
	if len(rows) != 100 || !stats.Truncated || len(stats.Warnings) != 0 {
		t.Errorf("limit 0: %d rows, stats %+v", len(rows), stats)
	}
}

// Sorting by a field GitHub orders by is sent as sort= and direction=, and
// the rows come back in that order.
func TestBrowseSortsThroughTheEndpoint(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.resetLog()
	cols, rows, _ := browse(t, c, issuesRef, driver.BrowseOptions{Sort: []driver.SortKey{{Field: "created_at", Desc: true}}})
	numbers := rowNumbers(t, cols, rows, "number")
	if len(numbers) != 100 || !slices.IsSortedFunc(numbers, func(a, b int64) int { return int(b - a) }) || numbers[0] != 250 {
		t.Errorf("created_at desc: numbers = %v", numbers)
	}
	for _, r := range f.requestsTo("/repos/octo/widgets/issues") {
		if r.Query.Get("sort") != "created" || r.Query.Get("direction") != "desc" {
			t.Errorf("query = %v, want sort=created direction=desc", r.Query)
		}
	}
	f.resetLog()
	cols, rows, _ = browse(t, c, issuesRef, driver.BrowseOptions{Sort: []driver.SortKey{{Field: "updated_at"}}})
	var prev time.Time
	for _, row := range rows {
		u, _ := cell(t, cols, row, "updated_at").AsTime()
		if u.Before(prev) {
			t.Fatalf("updated_at asc: %s comes after %s", u, prev)
		}
		prev = u
	}
	if reqs := f.requestsTo("/repos/octo/widgets/issues"); len(reqs) == 0 || reqs[0].Query.Get("sort") != "updated" || reqs[0].Query.Get("direction") != "asc" {
		t.Errorf("requests = %+v, want sort=updated direction=asc", reqs)
	}
	milestones := driver.CollectionRef{Namespace: "octo/widgets", Name: "milestones"}
	for _, tc := range []struct {
		desc      bool
		direction string
		titles    []string
	}{
		{false, "asc", []string{"v0.9", "v1.0", "v2.0"}},
		{true, "desc", []string{"v2.0", "v1.0", "v0.9"}},
	} {
		f.resetLog()
		cols, rows, _ := browse(t, c, milestones, driver.BrowseOptions{Sort: []driver.SortKey{{Field: "due_on", Desc: tc.desc}}})
		var titles []string
		for _, row := range rows {
			s, _ := cell(t, cols, row, "title").AsText()
			titles = append(titles, s)
		}
		if !reflect.DeepEqual(titles, tc.titles) {
			t.Errorf("milestones by due_on %s = %v, want %v", tc.direction, titles, tc.titles)
		}
		if reqs := f.requestsTo("/repos/octo/widgets/milestones"); len(reqs) != 1 || reqs[0].Query.Get("sort") != "due_on" || reqs[0].Query.Get("direction") != tc.direction {
			t.Errorf("requests = %+v", reqs)
		}
	}
}

// A sort GitHub does not offer and a raw filter are refused before any
// request, the sort refusal naming the sortable fields.
func TestBrowseRefusesUnsortableFieldsAndRawFilters(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.resetLog()
	_, err := c.Browse(context.Background(), issuesRef, driver.BrowseOptions{Sort: []driver.SortKey{{Field: "title"}}})
	wantKind(t, "sort by title", err, driver.ErrUnsupported)
	if !strings.Contains(err.Error(), `cannot sort issues by "title": it sorts by comments, created_at, updated_at`) {
		t.Errorf("sort refusal = %v", err)
	}
	_, err = c.Browse(context.Background(), driver.CollectionRef{Namespace: "octo/widgets", Name: "tags"}, driver.BrowseOptions{Sort: []driver.SortKey{{Field: "name"}}})
	wantKind(t, "sort tags", err, driver.ErrUnsupported)
	if !strings.Contains(err.Error(), "GitHub's own order only") {
		t.Errorf("tags sort refusal = %v", err)
	}
	_, err = c.Browse(context.Background(), issuesRef, driver.BrowseOptions{Filter: driver.Filter{Raw: "state = 'open'"}})
	wantKind(t, "raw filter", err, driver.ErrUnsupported)
	if !strings.Contains(err.Error(), "raw filter is not supported") {
		t.Errorf("raw filter refusal = %v", err)
	}
	if n := len(f.log()); n != 0 {
		t.Errorf("refused browses sent %d requests", n)
	}
}

// Each collection's own pushdowns reach its endpoint, and the rows satisfy
// the conditions GitHub applied.
func TestBrowsePushdownsPerCollection(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	t.Run("pull requests by head and base", func(t *testing.T) {
		f.resetLog()
		ref := driver.CollectionRef{Namespace: "octo/widgets", Name: "pull_requests"}
		cols, rows, _ := browse(t, c, ref, driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{
			{Field: "head", Op: driver.OpEq, Value: driver.String("feature/3")},
			{Field: "base", Op: driver.OpEq, Value: driver.String("main")},
		}}})
		if len(rows) != 1 {
			t.Fatalf("%d rows", len(rows))
		}
		if head, _ := cell(t, cols, rows[0], "head").AsText(); head != "feature/3" {
			t.Errorf("head = %q", head)
		}
		reqs := f.requestsTo("/repos/octo/widgets/pulls")
		if len(reqs) != 1 || reqs[0].Query.Get("head") != "feature/3" || reqs[0].Query.Get("base") != "main" || reqs[0].Query.Get("state") != "all" {
			t.Errorf("requests = %+v", reqs)
		}
	})
	t.Run("commits by author and date range", func(t *testing.T) {
		f.resetLog()
		since, until := seedBase.Add(60*time.Hour), seedBase.Add(100*time.Hour)
		ref := driver.CollectionRef{Namespace: "octo/widgets", Name: "commits"}
		cols, rows, _ := browse(t, c, ref, driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{
			{Field: "author", Op: driver.OpEq, Value: driver.String("octo")},
			{Field: "author_date", Op: driver.OpGe, Value: driver.Time(since)},
			{Field: "committer_date", Op: driver.OpLe, Value: driver.Time(until)},
		}}})
		want := 0
		for _, cm := range seededItems(f, "octo", "widgets", "commits") {
			d := tm(sub(sub(cm, "commit"), "author"), "date")
			if str(sub(cm, "author"), "login") == "octo" && !d.Before(since) && !d.After(until) {
				want++
			}
		}
		if want == 0 || len(rows) != want {
			t.Errorf("%d rows, want %d", len(rows), want)
		}
		for _, row := range rows {
			author, _ := cell(t, cols, row, "author").AsText()
			d, _ := cell(t, cols, row, "author_date").AsTime()
			if author != "octo" || d.Before(since) || d.After(until) {
				t.Errorf("row author=%s date=%s is outside the filter", author, d)
			}
		}
		reqs := f.requestsTo("/repos/octo/widgets/commits")
		if len(reqs) != 1 || reqs[0].Query.Get("author") != "octo" || reqs[0].Query.Get("since") != "2024-01-03T12:00:00Z" || reqs[0].Query.Get("until") != "2024-01-05T04:00:00Z" {
			t.Errorf("requests = %+v", reqs)
		}
	})
	t.Run("workflow runs by branch, status and creation", func(t *testing.T) {
		f.resetLog()
		from := seedBase.Add(745 * time.Hour)
		ref := driver.CollectionRef{Namespace: "octo/widgets", Name: "workflow_runs"}
		cols, rows, _ := browse(t, c, ref, driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{
			{Field: "head_branch", Op: driver.OpEq, Value: driver.String("main")},
			{Field: "status", Op: driver.OpEq, Value: driver.String("completed")},
			{Field: "created_at", Op: driver.OpGe, Value: driver.Time(from)},
		}}})
		want := 0
		for _, run := range seededItems(f, "octo", "widgets", "workflow_runs") {
			if str(run, "head_branch") == "main" && str(run, "status") == "completed" && !tm(run, "created_at").Before(from) {
				want++
			}
		}
		if want == 0 || len(rows) != want {
			t.Errorf("%d rows, want %d", len(rows), want)
		}
		for _, row := range rows {
			branch, _ := cell(t, cols, row, "head_branch").AsText()
			status, _ := cell(t, cols, row, "status").AsText()
			created, _ := cell(t, cols, row, "created_at").AsTime()
			if branch != "main" || status != "completed" || created.Before(from) {
				t.Errorf("row branch=%s status=%s created=%s is outside the filter", branch, status, created)
			}
		}
		reqs := f.requestsTo("/repos/octo/widgets/actions/runs")
		if len(reqs) != 1 || reqs[0].Query.Get("branch") != "main" || reqs[0].Query.Get("status") != "completed" || reqs[0].Query.Get("created") != ">=2024-02-01T01:00:00Z" {
			t.Errorf("requests = %+v", reqs)
		}
	})
	t.Run("protected branches", func(t *testing.T) {
		f.resetLog()
		ref := driver.CollectionRef{Namespace: "octo/widgets", Name: "branches"}
		cols, rows, _ := browse(t, c, ref, driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{
			{Field: "protected", Op: driver.OpEq, Value: driver.Bool(true)},
		}}})
		if len(rows) != 1 {
			t.Fatalf("%d rows", len(rows))
		}
		if name, _ := cell(t, cols, rows[0], "name").AsText(); name != "main" {
			t.Errorf("protected branch = %q", name)
		}
		reqs := f.requestsTo("/repos/octo/widgets/branches")
		if len(reqs) != 1 || reqs[0].Query.Get("protected") != "true" {
			t.Errorf("requests = %+v", reqs)
		}
	})
}

// A cursor is tied to the request it came from: the same page size with
// another condition sent to the endpoint, or another sort, is refused.
func TestBrowseRefusesTheCursorOfAnotherFilter(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	_, _, stats := browse(t, c, issuesRef, driver.BrowseOptions{Limit: 100})
	if stats.NextCursor == "" {
		t.Fatal("no cursor on a partial page")
	}
	for name, opts := range map[string]driver.BrowseOptions{
		"another condition": {Limit: 100, Cursor: stats.NextCursor, Filter: driver.Filter{Conditions: []driver.Condition{{Field: "state", Op: driver.OpEq, Value: driver.String("open")}}}},
		"another sort":      {Limit: 100, Cursor: stats.NextCursor, Sort: []driver.SortKey{{Field: "updated_at"}}},
	} {
		_, err := c.Browse(context.Background(), issuesRef, opts)
		wantKind(t, name, err, driver.ErrSyntax)
		if !strings.Contains(err.Error(), "other filters, sorting or page size") {
			t.Errorf("%s: refusal = %v", name, err)
		}
	}
}

// A missing repository or collection is ErrNotFound; a closed connection
// is ErrConnection.
func TestBrowseNotFoundAndClosed(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	_, err := c.Browse(context.Background(), driver.CollectionRef{Namespace: "octo/nothing", Name: "issues"}, driver.BrowseOptions{})
	wantKind(t, "a missing repository", err, driver.ErrNotFound)
	_, err = c.Browse(context.Background(), driver.CollectionRef{Namespace: "octo/widgets", Name: "widgets"}, driver.BrowseOptions{})
	wantKind(t, "a missing collection", err, driver.ErrNotFound)
	if !strings.Contains(err.Error(), "the collections are issues, pull_requests") {
		t.Errorf("the refusal does not list the collections: %v", err)
	}
	_, err = c.Browse(context.Background(), driver.CollectionRef{Namespace: "not-a-repo", Name: "issues"}, driver.BrowseOptions{})
	wantKind(t, "a namespace that is not owner/repo", err, driver.ErrNotFound)
	closed := f.open(t, false)
	_ = closed.Close()
	_, err = closed.Browse(context.Background(), issuesRef, driver.BrowseOptions{})
	wantKind(t, "a closed connection", err, driver.ErrConnection)
}

// Every Browse column names its collection as Source, with Key on the key
// column alone, so the TUI can edit rows inline; a low rate limit shows as
// a notice.
func TestBrowseColumnsCarryTheirSource(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	for _, col := range collections {
		ref := driver.CollectionRef{Namespace: "octo/widgets", Name: col.name}
		cols, _, _ := browse(t, c, ref, driver.BrowseOptions{Limit: 1})
		var keys []string
		for _, column := range cols {
			if column.Source == nil || *column.Source != ref {
				t.Errorf("%s.%s: Source = %v, want %v", col.name, column.Name, column.Source, ref)
			}
			if column.Key {
				keys = append(keys, column.Name)
			}
		}
		if !reflect.DeepEqual(keys, []string{col.keyCol}) {
			t.Errorf("%s: key columns = %v, want [%s]", col.name, keys, col.keyCol)
		}
	}
	f.setRate(5000, 100)
	_, _, stats := browse(t, c, issuesRef, driver.BrowseOptions{Limit: 1})
	if len(stats.Notices) != 1 || !strings.Contains(stats.Notices[0], "100 of 5000") {
		t.Errorf("notices = %v, want the low rate limit", stats.Notices)
	}
}
