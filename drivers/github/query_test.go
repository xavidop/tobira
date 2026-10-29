package github

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

// A plain list reads the first hundred issues, typed, newest first, with
// the pull requests the endpoint lists dropped, and reports more rows.
func TestListIssuesTypesRowsAndDropsPullRequests(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.resetLog()
	cols, rows, stats := queryText(t, c, `{"collection":"issues"}`)
	if len(rows) != 100 || !stats.Truncated {
		t.Fatalf("%d rows, truncated %v; want 100 and truncated", len(rows), stats.Truncated)
	}
	if stats.RowsAffected != -1 || len(stats.Warnings) != 0 {
		t.Errorf("stats = %+v", stats)
	}
	want := make([]string, len(issueColumns))
	for i, col := range issueColumns {
		want[i] = col.name
	}
	if got := columnNames(cols); !slices.Equal(got, want) {
		t.Errorf("columns = %v, want %v", got, want)
	}
	for _, col := range cols {
		if col.Source == nil || col.Source.Namespace != "octo/widgets" || col.Source.Name != "issues" || col.Key != (col.Name == "number") {
			t.Errorf("column %+v lacks its source or key", col)
		}
	}
	for i, row := range rows {
		number := cell(t, cols, row, "number")
		n, ok := number.AsInt()
		if !ok || n > 250 || n < 1 {
			t.Errorf("row %d number = %v, want an issue number (pull requests are 251-270)", i, number)
		}
		if created := cell(t, cols, row, "created_at"); created.Type() != driver.TypeTime {
			t.Errorf("row %d created_at = %v (%s), want a time", i, created, created.Type())
		}
		if labels := cell(t, cols, row, "labels"); labels.Type() != driver.TypeList {
			t.Errorf("row %d labels = %v (%s), want a list", i, labels, labels.Type())
		} else if n%2 == 0 {
			first, _ := labels.AsList()
			if len(first) == 0 {
				t.Errorf("row %d (#%d) has no labels, the seed gives even issues bug", i, n)
			} else if s, _ := first[0].AsText(); s != "bug" {
				t.Errorf("row %d labels[0] = %v, want bug", i, first[0])
			}
		}
		if user := cell(t, cols, row, "user"); user.Type() != driver.TypeString {
			t.Errorf("row %d user = %v, want the login", i, user)
		}
		if locked := cell(t, cols, row, "locked"); locked.Type() != driver.TypeBool {
			t.Errorf("row %d locked = %v", i, locked)
		}
		if n%3 == 0 {
			if state, _ := cell(t, cols, row, "state").AsText(); state != "closed" {
				t.Errorf("row %d (#%d) state = %q, want closed", i, n, state)
			}
			if closed := cell(t, cols, row, "closed_at"); closed.Type() != driver.TypeTime {
				t.Errorf("row %d closed_at = %v", i, closed)
			}
		} else if !cell(t, cols, row, "closed_at").IsNull() {
			t.Errorf("row %d (#%d) is open with a closed_at", i, n)
		}
	}
	first, _ := cell(t, cols, rows[0], "number").AsInt()
	if first != 250 {
		t.Errorf("first row is #%d, want the newest issue #250", first)
	}
	reqs := f.requestsTo("/repos/octo/widgets/issues")
	if len(reqs) != 2 || reqs[0].Query.Get("page") != "1" || reqs[1].Query.Get("page") != "2" || reqs[0].Query.Get("per_page") != "100" {
		t.Errorf("two pages should have been read (the first holds the 20 pull requests): %+v", reqs)
	}
}

// The endpoint's own parameters are sent as given; state defaults to all,
// so closed items are listed too.
func TestListSendsEndpointParametersAndDefaultsStateToAll(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.resetLog()
	cols, rows, _ := queryText(t, c, `{"collection":"issues","state":"open","labels":"bug,enhancement","assignee":"octo","sort":"comments","direction":"asc","since":"2024-01-01T00:00:00Z","limit":10}`)
	reqs := f.requestsTo("/repos/octo/widgets/issues")
	if len(reqs) == 0 {
		t.Fatal("no listing request")
	}
	q := reqs[0].Query
	// Issues are read in full pages whatever the limit, since the pull
	// requests among them are dropped after the read.
	for k, want := range map[string]string{"state": "open", "labels": "bug,enhancement", "assignee": "octo", "sort": "comments", "direction": "asc", "since": "2024-01-01T00:00:00Z", "per_page": "100", "page": "1"} {
		if q.Get(k) != want {
			t.Errorf("parameter %s = %q, want %q", k, q.Get(k), want)
		}
	}
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	for _, row := range rows {
		state, _ := cell(t, cols, row, "state").AsText()
		labels, _ := cell(t, cols, row, "labels").AsList()
		got := make([]string, len(labels))
		for i, l := range labels {
			got[i], _ = l.AsText()
		}
		if state != "open" || !slices.Contains(got, "bug") || !slices.Contains(got, "enhancement") {
			t.Errorf("row state=%s labels=%v does not match the parameters", state, got)
		}
	}

	f.resetLog()
	_, prs, _ := queryText(t, c, `{"collection":"pull_requests"}`)
	if reqs := f.requestsTo("/repos/octo/widgets/pulls"); len(reqs) != 1 || reqs[0].Query.Get("state") != "all" {
		t.Errorf("pull_requests requests = %+v, want state=all", reqs)
	}
	if len(prs) != 20 {
		t.Errorf("pull_requests: %d rows, want all 20 (open and closed)", len(prs))
	}
	f.resetLog()
	_, prs, stats := queryText(t, c, `{"collection":"pull_requests","state":"open","base":"main","limit":4}`)
	if reqs := f.requestsTo("/repos/octo/widgets/pulls"); len(reqs) != 1 || reqs[0].Query.Get("per_page") != "4" || reqs[0].Query.Get("base") != "main" {
		t.Errorf("pull_requests requests = %+v, want per_page=4", reqs)
	}
	if len(prs) != 4 || !stats.Truncated {
		t.Errorf("open pull requests with limit 4: %d rows, truncated %v", len(prs), stats.Truncated)
	}
	_, ms, _ := queryText(t, c, `{"collection":"milestones"}`)
	if len(ms) != 3 {
		t.Errorf("milestones: %d rows, want 3 with state=all", len(ms))
	}
	_, open, _ := queryText(t, c, `{"collection":"milestones","state":"open"}`)
	if len(open) != 2 {
		t.Errorf("open milestones: %d rows, want 2", len(open))
	}
	f.resetLog()
	_, runs, _ := queryText(t, c, `{"collection":"workflow_runs","branch":"main","status":"failure"}`)
	if reqs := f.requestsTo("/repos/octo/widgets/actions/runs"); len(reqs) != 1 || reqs[0].Query.Get("branch") != "main" || reqs[0].Query.Get("status") != "failure" {
		t.Errorf("workflow_runs requests = %+v", reqs)
	}
	if len(runs) == 0 {
		t.Error("no failed runs on main")
	}
	_, err := c.Query(context.Background(), driver.Query{Text: `{"collection":"issues","head":"main"}`})
	wantKind(t, "a parameter of another collection", err, driver.ErrSyntax)
}

// A limit over the cap is cut to 1000 rows with a warning; the listing
// ending sooner is not truncation.
func TestListLimitIsCutWithAWarning(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.resetLog()
	_, rows, stats := queryText(t, c, `{"collection":"issues","limit":5000}`)
	if len(rows) != 250 || stats.Truncated {
		t.Errorf("%d rows, truncated %v; want all 250 issues, not truncated", len(rows), stats.Truncated)
	}
	if len(stats.Warnings) != 1 || !strings.Contains(stats.Warnings[0], "5000") || !strings.Contains(stats.Warnings[0], "1000") {
		t.Errorf("warnings = %v, want the cut", stats.Warnings)
	}
	if reqs := f.requestsTo("/repos/octo/widgets/issues"); len(reqs) != 3 {
		t.Errorf("%d requests for 270 items of 100, want 3", len(reqs))
	}
	_, rows, stats = queryText(t, c, `{"collection":"issues","limit":1000}`)
	if len(rows) != 250 || len(stats.Warnings) != 0 {
		t.Errorf("limit 1000: %d rows, warnings %v", len(rows), stats.Warnings)
	}
}

// Query.Limit cuts the rows further and reports truncation.
func TestQueryLimitTruncates(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	_, rows, stats := queryWith(t, c, driver.Query{Text: `{"collection":"labels"}`, Limit: 3})
	if len(rows) != 3 || !stats.Truncated {
		t.Errorf("%d rows, truncated %v; want 3 and truncated", len(rows), stats.Truncated)
	}
	_, rows, stats = queryWith(t, c, driver.Query{Text: `{"collection":"labels"}`, Limit: 50})
	if len(rows) != 7 || stats.Truncated {
		t.Errorf("%d rows, truncated %v; want all 7, not truncated", len(rows), stats.Truncated)
	}
	_, rows, stats = queryWith(t, c, driver.Query{Text: `{"collection":"labels","limit":2}`, Limit: 5})
	if len(rows) != 2 || !stats.Truncated {
		t.Errorf("limit 2 under Query.Limit 5: %d rows, truncated %v", len(rows), stats.Truncated)
	}
}

// The repository comes from the "repo" member, else Query.Namespace, else
// the DSN; without a pinned repository the DSN's owner is the scope.
func TestListRepositoryResolution(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.resetLog()
	cols, rows, _ := queryWith(t, c, driver.Query{Text: `{"collection":"issues"}`, Namespace: "acme/alpha"})
	if len(rows) != 4 || cols[0].Source.Namespace != "acme/alpha" {
		t.Errorf("Query.Namespace: %d rows from %+v", len(rows), cols[0].Source)
	}
	if len(f.requestsTo("/repos/acme/alpha/issues")) != 1 || len(f.requestsTo("/repos/octo/widgets/issues")) != 0 {
		t.Errorf("requests = %+v", f.log())
	}
	f.resetLog()
	_, rows, _ = queryWith(t, c, driver.Query{Text: `{"collection":"issues","repo":"octo/gadgets"}`, Namespace: "acme/alpha"})
	if len(rows) != 3 || len(f.requestsTo("/repos/octo/gadgets/issues")) != 1 || len(f.requestsTo("/repos/acme/alpha/issues")) != 0 {
		t.Errorf(`"repo" should win over Query.Namespace: %d rows, %+v`, len(rows), f.log())
	}
	f.resetLog()
	_, rows, _ = queryText(t, c, `{"collection":"issues","limit":5}`)
	if len(rows) != 5 || len(f.requestsTo("/repos/octo/widgets/issues")) == 0 {
		t.Errorf("the DSN repository should be the default: %d rows, %+v", len(rows), f.log())
	}
	_, err := c.Query(context.Background(), driver.Query{Text: `{"collection":"issues","repo":"nobody/nothing"}`})
	wantKind(t, "a missing repository", err, driver.ErrNotFound)
	_, err = c.Query(context.Background(), driver.Query{Text: `{"collection":"issues","repo":"not-a-repo"}`})
	wantKind(t, "a repo that is an unknown owner", err, driver.ErrNotFound)
	if de := asDriverError(t, err); de.Code != "404" {
		t.Errorf("an unknown owner has code %q, want 404", de.Code)
	}
	_, err = c.Query(context.Background(), driver.Query{Text: `{"collection":"issues","repo":"a/b/c"}`})
	wantKind(t, "a repo that is neither owner/repo nor an owner", err, driver.ErrSyntax)
	_, err = c.Query(context.Background(), driver.Query{Text: `{"collection":"issues","repo":"-bad"}`})
	wantKind(t, "a repo that is not a valid owner", err, driver.ErrSyntax)

	owner := f.openDSN(t, f.dsn("acme", ""), false, fakeToken)
	f.resetLog()
	cols, rows, _ = queryWith(t, owner, driver.Query{Text: `{"collection":"issues"}`})
	if len(rows) != 5 || cols[0].Source.Namespace != "acme" || len(f.requestsTo("/search/issues")) != 1 {
		t.Errorf("a list without any repository is the owner's: %d rows from %+v, %+v", len(rows), cols[0].Source, f.log())
	}
	_, rows, _ = queryWith(t, owner, driver.Query{Text: `{"collection":"issues"}`, Namespace: "acme/beta"})
	if len(rows) != 1 {
		t.Errorf("acme/beta through the namespace: %d rows", len(rows))
	}
}

// get reads one item of each keyed collection through its own endpoint.
func TestGetEachKeyedCollection(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	commit := f.find("octo", "widgets", "commits", func(object) bool { return true })
	comment := f.find("octo", "widgets", "issue_comments", func(object) bool { return true })
	release := f.find("octo", "widgets", "releases", func(object) bool { return true })
	run := f.find("octo", "widgets", "workflow_runs", func(object) bool { return true })
	for _, tc := range []struct {
		text, path, keyCol string
		want               driver.Value
		check              func(cols []driver.Column, row driver.Row) string
	}{
		{`{"op":"get","collection":"issues","number":7}`, "/repos/octo/widgets/issues/7", "number", driver.Int(7), func(cols []driver.Column, row driver.Row) string {
			title, _ := cell(t, cols, row, "title").AsText()
			labels, _ := cell(t, cols, row, "labels").AsList()
			ms, _ := cell(t, cols, row, "milestone").AsText()
			if title != "Issue 7" || len(labels) != 1 || ms != "v1.0" {
				return "title " + title + ", milestone " + ms
			}
			return ""
		}},
		{`{"op":"get","collection":"issues","key":"7"}`, "/repos/octo/widgets/issues/7", "number", driver.Int(7), nil},
		{`{"op":"get","collection":"pull_requests","number":252}`, "/repos/octo/widgets/pulls/252", "number", driver.Int(252), func(cols []driver.Column, row driver.Row) string {
			head, _ := cell(t, cols, row, "head").AsText()
			base, _ := cell(t, cols, row, "base").AsText()
			reviewers, _ := cell(t, cols, row, "requested_reviewers").AsList()
			if head != "feature/2" || base != "main" || len(reviewers) != 1 || cell(t, cols, row, "merged_at").Type() != driver.TypeTime {
				return "head " + head + ", base " + base
			}
			return ""
		}},
		{`{"op":"get","collection":"issue_comments","id":` + itoa(num(comment, "id")) + `}`, "/repos/octo/widgets/issues/comments/" + itoa(num(comment, "id")), "id", driver.Int(num(comment, "id")), func(cols []driver.Column, row driver.Row) string {
			if issue := cell(t, cols, row, "issue"); issue.Type() != driver.TypeInt {
				return "issue " + issue.String()
			}
			return ""
		}},
		{`{"op":"get","collection":"commits","sha":"` + str(commit, "sha") + `"}`, "/repos/octo/widgets/commits/" + str(commit, "sha"), "sha", driver.String(str(commit, "sha")), func(cols []driver.Column, row driver.Row) string {
			parents, _ := cell(t, cols, row, "parents").AsList()
			if cell(t, cols, row, "author_date").Type() != driver.TypeTime || len(parents) != 1 || cell(t, cols, row, "committer").Type() != driver.TypeString {
				return "author_date " + cell(t, cols, row, "author_date").String()
			}
			return ""
		}},
		{`{"op":"get","collection":"branches","name":"feature/1"}`, "/repos/octo/widgets/branches/feature/1", "name", driver.String("feature/1"), nil},
		{`{"op":"get","collection":"branches","name":"main"}`, "/repos/octo/widgets/branches/main", "name", driver.String("main"), func(cols []driver.Column, row driver.Row) string {
			if p, _ := cell(t, cols, row, "protected").AsBool(); !p {
				return "main is not protected"
			}
			return ""
		}},
		{`{"op":"get","collection":"releases","id":` + itoa(num(release, "id")) + `}`, "/repos/octo/widgets/releases/" + itoa(num(release, "id")), "id", driver.Int(num(release, "id")), func(cols []driver.Column, row driver.Row) string {
			if assets := cell(t, cols, row, "assets"); assets.Type() != driver.TypeInt {
				return "assets " + assets.String()
			}
			return ""
		}},
		{`{"op":"get","collection":"labels","name":"good first issue"}`, "/repos/octo/widgets/labels/good first issue", "name", driver.String("good first issue"), func(cols []driver.Column, row driver.Row) string {
			if d, _ := cell(t, cols, row, "default").AsBool(); !d {
				return "default false"
			}
			return ""
		}},
		{`{"op":"get","collection":"milestones","number":3}`, "/repos/octo/widgets/milestones/3", "number", driver.Int(3), func(cols []driver.Column, row driver.Row) string {
			if cell(t, cols, row, "closed_at").Type() != driver.TypeTime || cell(t, cols, row, "due_on").Type() != driver.TypeTime {
				return "closed_at " + cell(t, cols, row, "closed_at").String()
			}
			return ""
		}},
		{`{"op":"get","collection":"workflow_runs","id":` + itoa(num(run, "id")) + `}`, "/repos/octo/widgets/actions/runs/" + itoa(num(run, "id")), "id", driver.Int(num(run, "id")), func(cols []driver.Column, row driver.Row) string {
			if actor := cell(t, cols, row, "actor"); actor.Type() != driver.TypeString {
				return "actor " + actor.String()
			}
			return ""
		}},
	} {
		f.resetLog()
		cols, rows, _ := queryText(t, c, tc.text)
		if len(rows) != 1 {
			t.Errorf("%s: %d rows", tc.text, len(rows))
			continue
		}
		if got := cell(t, cols, rows[0], tc.keyCol); got != tc.want {
			t.Errorf("%s: key = %v, want %v", tc.text, got, tc.want)
		}
		if tc.check != nil {
			if msg := tc.check(cols, rows[0]); msg != "" {
				t.Errorf("%s: %s", tc.text, msg)
			}
		}
		if reqs := f.requestsTo(tc.path); len(reqs) != 1 {
			t.Errorf("%s: requests to %s = %d (log %+v)", tc.text, tc.path, len(reqs), f.log())
		}
	}
	_, err := c.Query(context.Background(), driver.Query{Text: `{"op":"get","collection":"issues","id":7}`})
	wantKind(t, "an issue keyed by id", err, driver.ErrSyntax)
	_, err = c.Query(context.Background(), driver.Query{Text: `{"op":"get","collection":"issues"}`})
	wantKind(t, "a get without a key", err, driver.ErrSyntax)
}

func itoa(n int64) string { return driver.Int(n).String() }

// Tags have no single-item endpoint: a get searches the listing.
func TestGetTagScansTheListing(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.resetLog()
	cols, rows, _ := queryText(t, c, `{"op":"get","collection":"tags","name":"v1.0.0"}`)
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	if name, _ := cell(t, cols, rows[0], "name").AsText(); name != "v1.0.0" {
		t.Errorf("name = %q", name)
	}
	if z, _ := cell(t, cols, rows[0], "zipball_url").AsText(); !strings.HasSuffix(z, "v1.0.0") {
		t.Errorf("zipball_url = %q", z)
	}
	reqs := f.requestsTo("/repos/octo/widgets/tags")
	if len(reqs) != 1 || reqs[0].Query.Get("per_page") != "100" {
		t.Errorf("requests = %+v, want one listing page", reqs)
	}
	_, err := c.Query(context.Background(), driver.Query{Text: `{"op":"get","collection":"tags","name":"v9.9.9"}`})
	wantKind(t, "a missing tag", err, driver.ErrNotFound)
}

// A pull request number read through issues is not an issue.
func TestGetPullRequestThroughIssuesIsErrNotFound(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	_, err := c.Query(context.Background(), driver.Query{Text: `{"op":"get","collection":"issues","number":251}`})
	wantKind(t, "a pull request number through issues", err, driver.ErrNotFound)
	if !strings.Contains(err.Error(), "pull_requests") {
		t.Errorf("the error does not point at pull_requests: %v", err)
	}
	_, rows, _ := queryText(t, c, `{"op":"get","collection":"pull_requests","number":251}`)
	if len(rows) != 1 {
		t.Errorf("the same number through pull_requests: %d rows", len(rows))
	}
}

// Searches go to /search/{type}; issue results carry the repository and
// whether the item is a pull request.
func TestSearchIssues(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.resetLog()
	cols, rows, stats := queryText(t, c, `{"op":"search","type":"issues","q":"repo:octo/widgets is:open label:bug","sort":"updated","order":"asc","limit":10}`)
	if len(rows) != 10 || !stats.Truncated {
		t.Fatalf("%d rows, truncated %v; want 10 of many", len(rows), stats.Truncated)
	}
	reqs := f.requestsTo("/search/issues")
	if len(reqs) != 1 || reqs[0].Query.Get("q") != "repo:octo/widgets is:open label:bug" || reqs[0].Query.Get("sort") != "updated" || reqs[0].Query.Get("order") != "asc" || reqs[0].Query.Get("per_page") != "10" {
		t.Errorf("requests = %+v", reqs)
	}
	for _, row := range rows {
		repo, _ := cell(t, cols, row, "repository").AsText()
		pr, _ := cell(t, cols, row, "pull_request").AsBool()
		state, _ := cell(t, cols, row, "state").AsText()
		if repo != "octo/widgets" || pr || state != "open" {
			t.Errorf("row repository=%s pull_request=%v state=%s", repo, pr, state)
		}
	}
	for _, col := range cols {
		if col.Source != nil {
			t.Errorf("search column %s has a source; search rows are not editable", col.Name)
		}
	}
	first, _ := cell(t, cols, rows[0], "updated_at").AsTime()
	last, _ := cell(t, cols, rows[9], "updated_at").AsTime()
	if !first.Before(last) {
		t.Errorf("sort=updated order=asc gave %v before %v", first, last)
	}
	_, err := c.Query(context.Background(), driver.Query{Text: `{"op":"search","type":"issues","q":"  "}`})
	wantKind(t, "a search without q", err, driver.ErrSyntax)
	_, err = c.Query(context.Background(), driver.Query{Text: `{"op":"search","type":"gists","q":"x"}`})
	wantKind(t, "an unknown search type", err, driver.ErrSyntax)
}

// A pull_requests search is an issues search with is:pr appended.
func TestSearchPullRequests(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.resetLog()
	cols, rows, stats := queryText(t, c, `{"op":"search","type":"pull_requests","q":"repo:octo/widgets improve"}`)
	if len(rows) != 20 || stats.Truncated {
		t.Fatalf("%d rows, truncated %v; want the 20 pull requests", len(rows), stats.Truncated)
	}
	reqs := f.requestsTo("/search/issues")
	if len(reqs) != 1 || reqs[0].Query.Get("q") != "repo:octo/widgets improve is:pr" {
		t.Errorf("requests = %+v", reqs)
	}
	for _, row := range rows {
		if pr, _ := cell(t, cols, row, "pull_request").AsBool(); !pr {
			t.Errorf("row %v is not a pull request", row)
		}
	}
}

// A repositories search returns repository rows keyed by full_name.
func TestSearchRepositories(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	cols, rows, _ := queryText(t, c, `{"op":"search","type":"repositories","q":"user:octo"}`)
	if len(rows) != 2 {
		t.Fatalf("%d rows, want octo's two public repositories", len(rows))
	}
	var names []string
	for _, row := range rows {
		s, _ := cell(t, cols, row, "full_name").AsText()
		names = append(names, s)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"octo/gadgets", "octo/widgets"}) {
		t.Errorf("full_name = %v", names)
	}
	for _, row := range rows {
		if owner, _ := cell(t, cols, row, "owner").AsText(); owner != "octo" {
			t.Errorf("owner = %q", owner)
		}
		if stars := cell(t, cols, row, "stars"); stars.Type() != driver.TypeInt {
			t.Errorf("stars = %v", stars)
		}
		if topics := cell(t, cols, row, "topics"); topics.Type() != driver.TypeList {
			t.Errorf("topics = %v", topics)
		}
	}
}

// A commits search carries the repository of each commit.
func TestSearchCommits(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	cols, rows, _ := queryText(t, c, `{"op":"search","type":"commits","q":"repo:octo/widgets typo"}`)
	if len(rows) != 15 {
		t.Fatalf("%d rows, want the 15 typo fixes", len(rows))
	}
	for _, row := range rows {
		repo, _ := cell(t, cols, row, "repository").AsText()
		msg, _ := cell(t, cols, row, "message").AsText()
		if repo != "octo/widgets" || !strings.Contains(msg, "typo") {
			t.Errorf("row repository=%s message=%q", repo, msg)
		}
	}
}

// The repositories op lists the connection's repositories with their
// details: the pinned one alone, or the owner's.
func TestRepositoriesOp(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	cols, rows, stats := queryText(t, c, `{"op":"repositories"}`)
	if len(rows) != 1 || stats.Truncated {
		t.Fatalf("pinned: %d rows", len(rows))
	}
	if full, _ := cell(t, cols, rows[0], "full_name").AsText(); full != "octo/widgets" || !cols[colIndex(t, cols, "full_name")].Key {
		t.Errorf("row = %v", rows[0])
	}
	if open, _ := cell(t, cols, rows[0], "open_issues").AsInt(); open != f.openIssues("octo", "widgets") {
		t.Errorf("open_issues = %d", open)
	}
	if branch, _ := cell(t, cols, rows[0], "default_branch").AsText(); branch != "main" {
		t.Errorf("default_branch = %q", branch)
	}

	org := f.openDSN(t, f.dsn("acme", ""), false, fakeToken)
	cols, rows, stats = queryText(t, org, `{"op":"repositories"}`)
	if len(rows) != 2 || stats.Truncated {
		t.Fatalf("organization: %d rows, truncated %v", len(rows), stats.Truncated)
	}
	if private, _ := cell(t, cols, rows[1], "private").AsBool(); !private {
		t.Errorf("acme/beta should be private: %v", rows[1])
	}
	_, rows, _ = queryText(t, org, `{"op":"repositories","limit":1}`)
	if len(rows) != 1 {
		t.Errorf("limit 1: %d rows", len(rows))
	}
}

// A rest GET of an array is one row per element over the union of their
// members, following the Link header up to the limit.
func TestRestGetArrayFollowsPagination(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.resetLog()
	cols, rows, stats := queryText(t, c, `{"op":"rest","path":"/repos/octo/widgets/labels","limit":3}`)
	if len(rows) != 3 || !stats.Truncated {
		t.Fatalf("%d rows, truncated %v; want 3 of 7", len(rows), stats.Truncated)
	}
	for _, name := range []string{"id", "name", "color", "description", "default", "url"} {
		colIndex(t, cols, name)
	}
	if name, _ := cell(t, cols, rows[0], "name").AsText(); name != "bug" {
		t.Errorf("first label = %q", name)
	}
	if cols[colIndex(t, cols, "default")].Type != driver.TypeBool || cols[colIndex(t, cols, "id")].Type != driver.TypeInt {
		t.Errorf("column types = %+v", cols)
	}
	reqs := f.requestsTo("/repos/octo/widgets/labels")
	if len(reqs) != 2 || reqs[0].Query.Get("per_page") != "3" || reqs[1].Query.Get("page") != "2" {
		t.Errorf("requests = %+v, want page 1 then page 2 of 3", reqs)
	}

	f.resetLog()
	_, rows, stats = queryText(t, c, `{"op":"rest","method":"GET","path":"/repos/octo/widgets/labels"}`)
	if len(rows) != 7 || stats.Truncated || len(f.requestsTo("/repos/octo/widgets/labels")) != 1 {
		t.Errorf("one short page: %d rows, truncated %v, %d requests", len(rows), stats.Truncated, len(f.requestsTo("/repos/octo/widgets/labels")))
	}
	f.resetLog()
	_, rows, _ = queryText(t, c, `{"op":"rest","path":"/repos/octo/widgets/issues","params":{"state":"closed","per_page":5,"page":2}}`)
	reqs = f.requestsTo("/repos/octo/widgets/issues")
	if len(rows) != 5 || len(reqs) != 1 || reqs[0].Query.Get("state") != "closed" || reqs[0].Query.Get("page") != "2" || reqs[0].Query.Get("per_page") != "5" {
		t.Errorf("an explicit page is not followed: %d rows, %+v", len(rows), reqs)
	}
	_, err := c.Query(context.Background(), driver.Query{Text: `{"op":"rest","path":"repos/octo/widgets"}`})
	wantKind(t, "a path without a leading slash", err, driver.ErrSyntax)
	_, err = c.Query(context.Background(), driver.Query{Text: `{"op":"rest","path":"/repos/octo/widgets?x=1"}`})
	wantKind(t, "a path with a query", err, driver.ErrSyntax)
	_, err = c.Query(context.Background(), driver.Query{Text: `{"op":"rest","path":"/repos/../user"}`})
	wantKind(t, "a path with a parent segment", err, driver.ErrSyntax)
	_, err = c.Query(context.Background(), driver.Query{Text: `{"op":"rest","path":"/repos/octo/widgets","body":{"x":1}}`})
	wantKind(t, "a GET with a body", err, driver.ErrSyntax)
}

// A rest GET of an object is one row, a column per member in key order.
func TestRestGetObject(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	cols, rows, stats := queryText(t, c, `{"op":"rest","path":"/repos/octo/widgets"}`)
	if len(rows) != 1 || stats.Truncated || len(stats.Notices) != 0 {
		t.Fatalf("%d rows, stats %+v", len(rows), stats)
	}
	if !slices.IsSortedFunc(cols, func(a, b driver.Column) int { return strings.Compare(a.Name, b.Name) }) {
		t.Errorf("columns are not in key order: %v", columnNames(cols))
	}
	if full, _ := cell(t, cols, rows[0], "full_name").AsText(); full != "octo/widgets" {
		t.Errorf("full_name = %q", full)
	}
	if owner := cell(t, cols, rows[0], "owner"); owner.Type() != driver.TypeJSON {
		t.Errorf("a nested member should be JSON: %v (%s)", owner, owner.Type())
	}
	if private := cell(t, cols, rows[0], "private"); private.Type() != driver.TypeBool {
		t.Errorf("private = %v", private)
	}
	cols, rows, _ = queryText(t, c, `{"op":"rest","path":"/repos/octo/widgets/topics"}`)
	if len(rows) != 1 || cell(t, cols, rows[0], "names").Type() != driver.TypeJSON {
		t.Errorf("topics = %v", rows)
	}
}

// A rest POST on a writable connection creates the item and reports the
// status as a notice.
func TestRestPostOnWritableConnection(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	cols, rows, stats := queryText(t, c, `{"op":"rest","method":"POST","path":"/repos/octo/widgets/labels","body":{"name":"triage","color":"ff0000","description":"needs a look"}}`)
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	if name, _ := cell(t, cols, rows[0], "name").AsText(); name != "triage" {
		t.Errorf("name = %q", name)
	}
	if len(stats.Notices) != 1 || !strings.Contains(stats.Notices[0], "201") {
		t.Errorf("notices = %v, want 201 Created", stats.Notices)
	}
	l := f.find("octo", "widgets", "labels", func(o object) bool { return str(o, "name") == "triage" })
	if l == nil || str(l, "color") != "ff0000" {
		t.Errorf("the label was not created: %v", l)
	}
	w := f.writes()
	if len(w) != 1 || w[0].Header.Get("Content-Type") != "application/json" || !strings.Contains(string(w[0].Body), `"triage"`) {
		t.Errorf("writes = %+v", w)
	}
}

// A 204 answer is no rows and a notice.
func TestRestNoContentHasNoRows(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	cols, rows, stats := queryText(t, c, `{"op":"rest","method":"DELETE","path":"/repos/octo/widgets/labels/wontfix"}`)
	if len(rows) != 0 || len(cols) != 0 {
		t.Errorf("%d columns, %d rows; want none", len(cols), len(rows))
	}
	if len(stats.Notices) != 1 || !strings.Contains(stats.Notices[0], "204") {
		t.Errorf("notices = %v, want 204 No Content", stats.Notices)
	}
	if f.find("octo", "widgets", "labels", func(o object) bool { return str(o, "name") == "wontfix" }) != nil {
		t.Error("the label is still there")
	}
}

// A GraphQL query is one row with a column per top-level data field.
func TestGraphQLQueryRow(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	cols, rows, stats := queryText(t, c, `{"op":"graphql","query":"query($n: Int!) { viewer { login } }","variables":{"n":1}}`)
	if len(rows) != 1 || len(cols) != 1 || cols[0].Name != "viewer" || cols[0].Type != driver.TypeJSON {
		t.Fatalf("columns %+v, %d rows", cols, len(rows))
	}
	raw, _ := rows[0][0].AsJSON()
	if !strings.Contains(string(raw), `"octo"`) {
		t.Errorf("viewer = %s", raw)
	}
	if len(stats.Warnings) != 0 {
		t.Errorf("warnings = %v", stats.Warnings)
	}
	reqs := f.requestsTo("/graphql")
	if len(reqs) != 1 || reqs[0].Method != http.MethodPost || !strings.Contains(string(reqs[0].Body), `"variables":{"n":1}`) {
		t.Errorf("requests = %+v", reqs)
	}
	_, err := c.Query(context.Background(), driver.Query{Text: `{"op":"graphql","query":"{ viewer { login } }","variables":[1]}`})
	wantKind(t, "variables that are not an object", err, driver.ErrSyntax)
	_, err = c.Query(context.Background(), driver.Query{Text: `{"op":"graphql","query":"  "}`})
	wantKind(t, "an empty document", err, driver.ErrSyntax)
}

// GraphQL errors without data fail the query as ErrSyntax with the message.
func TestGraphQLErrorsAreErrSyntax(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	_, err := c.Query(context.Background(), driver.Query{Text: `{"op":"graphql","query":"{ nope }"}`})
	wantKind(t, "a GraphQL error", err, driver.ErrSyntax)
	if !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), "undefinedField") {
		t.Errorf("the error lacks the server's message: %v", err)
	}
}

// rate_limit is one row per resource, sorted, with the reset as a time.
func TestRateLimitRows(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	cols, rows, _ := queryText(t, c, `{"op":"rate_limit"}`)
	var names []string
	for _, row := range rows {
		s, _ := cell(t, cols, row, "resource").AsText()
		names = append(names, s)
	}
	if !slices.Equal(names, []string{"core", "graphql", "search"}) {
		t.Errorf("resources = %v", names)
	}
	limit, _ := cell(t, cols, rows[0], "limit").AsInt()
	remaining, _ := cell(t, cols, rows[0], "remaining").AsInt()
	reset, ok := cell(t, cols, rows[0], "reset").AsTime()
	if limit != 5000 || remaining != 4987 || !ok || reset != time.Unix(1704070800, 0).UTC() {
		t.Errorf("core = %v", rows[0])
	}
}

// Queries that cannot be run are refused before anything is sent.
func TestQueryRefusals(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.resetLog()
	_, err := c.Query(context.Background(), driver.Query{Text: `{"collection":"people"}`})
	wantKind(t, "a missing collection", err, driver.ErrNotFound)
	_, err = c.Query(context.Background(), driver.Query{Text: `{"op":"get","collection":"people","number":1}`})
	wantKind(t, "a get on a missing collection", err, driver.ErrNotFound)
	_, err = c.Query(context.Background(), driver.Query{Text: `{"collection":"issues"}`, Params: []driver.Value{driver.Int(1)}})
	wantKind(t, "positional parameters", err, driver.ErrSyntax)
	_, err = c.Query(context.Background(), driver.Query{Text: `{"op":"list"}`})
	wantKind(t, "a list without a collection", err, driver.ErrSyntax)
	_, err = c.Query(context.Background(), driver.Query{Text: `{"collection":"issues","limit":-1}`})
	wantKind(t, "a negative limit", err, driver.ErrSyntax)
	_, err = c.Query(context.Background(), driver.Query{Text: `{"collection":"issues","limit":"ten"}`})
	wantKind(t, "a text limit", err, driver.ErrSyntax)
	_, err = c.Query(context.Background(), driver.Query{Text: `{"op":"rest","method":"TRACE","path":"/"}`})
	wantKind(t, "an unknown method", err, driver.ErrSyntax)
	_, err = c.Query(context.Background(), driver.Query{Text: `{"op":"rate_limit","limit":1}`})
	wantKind(t, "a member the op does not take", err, driver.ErrSyntax)
	_, err = c.Query(context.Background(), driver.Query{Text: `select 1`})
	wantKind(t, "SQL", err, driver.ErrSyntax)
	if n := len(f.log()); n != 0 {
		t.Errorf("refused queries sent %d requests: %+v", n, f.log())
	}
}

// A context that is already done fails with its error and sends nothing.
func TestPreCancelledContext(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.resetLog()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Query(ctx, driver.Query{Text: `{"collection":"labels"}`})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Query with a cancelled context: %v, want context.Canceled", err)
	}
	if err := c.Ping(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Ping with a cancelled context: %v", err)
	}
	if _, err := c.Namespaces(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Namespaces with a cancelled context: %v", err)
	}
	if n := len(f.log()); n != 0 {
		t.Errorf("%d requests were sent with a cancelled context", n)
	}
	if _, rows, _ := queryText(t, c, `{"collection":"labels"}`); len(rows) != 7 {
		t.Errorf("the connection is unusable after a cancelled call: %d rows", len(rows))
	}
}

// Cancelling while the server holds the request fails with
// context.Canceled, promptly.
func TestCancellationMidRequest(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.setHook(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path != "/repos/octo/widgets/labels" {
			return false
		}
		<-r.Context().Done()
		return true
	})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	start := time.Now()
	_, err := c.Query(ctx, driver.Query{Text: `{"collection":"labels"}`})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Query cancelled mid-request: %v, want context.Canceled", err)
	}
	if driver.KindOf(err) != nil {
		t.Errorf("a cancellation is classified as %v", driver.KindOf(err))
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("the cancellation took %s", time.Since(start))
	}
	f.setHook(nil)
	if _, rows, _ := queryText(t, c, `{"collection":"labels"}`); len(rows) != 7 {
		t.Errorf("the connection is unusable after a cancelled request: %d rows", len(rows))
	}
}

// A deadline that passes mid-request is context.DeadlineExceeded.
func TestDeadlineMidRequest(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.setHook(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path != "/repos/octo/widgets/labels" {
			return false
		}
		<-r.Context().Done()
		return true
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := c.Query(ctx, driver.Query{Text: `{"collection":"labels"}`})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Query past its deadline: %v, want context.DeadlineExceeded", err)
	}
}
