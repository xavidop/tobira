package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

// An owner namespace (a user or an organization, no slash) reads every
// repository of the owner at once: its repositories through the listing
// endpoints, its issues, pull requests and commits through GitHub search.
// These tests run that through the fake.

// seedIssues adds n open issues to a repository of the fake, titled after
// the repository, so a listing can span several search pages.
func seedIssues(f *fakeGitHub, owner, repo string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rp := f.findRepo(owner, repo)
	for i := 0; i < n; i++ {
		f.newIssue(rp, owner, fmt.Sprintf("%s extra %d", repo, i+1), "", nil, nil, nil, at(100+i))
	}
}

// seedAcmeVariety adds to acme/alpha one issue assigned to hubot, with the
// milestone v1, five comments and a later creation, and closes alpha's
// first issue, so every search qualifier has something to match.
func seedAcmeVariety(f *fakeGitHub) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rp := f.findRepo("acme", "alpha")
	ms := f.addMilestone(rp, "v1", "first", "open", at(900))
	is := f.newIssue(rp, "hubot", "alpha needs a plan", "A plan.", []string{"bug"}, []string{"hubot"}, ms, at(10))
	is["comments"] = 5
	is["updated_at"] = at(20)
	first := rp.issues[0]
	first["state"] = "closed"
	first["closed_at"] = at(6)
}

// repositoriesOf reads the repository column of every row.
func repositoriesOf(t *testing.T, cols []driver.Column, rows []driver.Row) []string {
	t.Helper()
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i], _ = cell(t, cols, row, "repository").AsText()
	}
	return out
}

// sameTerms reports two search texts made of the same terms in the same
// order, whatever the spacing between them.
func sameTerms(got, want string) bool {
	return slices.Equal(strings.Fields(got), strings.Fields(want))
}

// searchQueries lists the q of every request to a search endpoint.
func searchQueries(f *fakeGitHub, endpoint string) []string {
	var out []string
	for _, r := range f.requestsTo(endpoint) {
		out = append(out, r.Query.Get("q"))
	}
	return out
}

// Catalog.

// An owner's collections are the four owner-wide ones, in the owner's
// namespace, without row estimates; a missing owner is ErrNotFound with the
// 404 as its code.
func TestCollectionsOfAnOwner(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	ctx := context.Background()
	for _, owner := range []string{"acme", "hubot", "octo"} {
		cols, err := c.Collections(ctx, driver.NamespaceRef{Name: owner})
		if err != nil {
			t.Fatalf("%s: %v", owner, err)
		}
		if got := collectionRefNames(cols); !slices.Equal(got, []string{"repositories", "issues", "pull_requests", "commits"}) {
			t.Errorf("%s: Collections = %v", owner, got)
		}
		for _, col := range cols {
			if col.Ref.Namespace != owner || col.Type != driver.CollectionTable || col.RowEstimate != -1 || col.Comment == "" {
				t.Errorf("%s: collection %+v", owner, col)
			}
		}
	}
	// The owner's login is spelled as GitHub does.
	cols, err := c.Collections(ctx, driver.NamespaceRef{Name: "ACME"})
	if err != nil || cols[0].Ref.Namespace != "acme" {
		t.Errorf("Collections of ACME = %+v, %v", cols, err)
	}
	_, err = c.Collections(ctx, driver.NamespaceRef{Name: "nobody"})
	wantKind(t, "Collections of an unknown owner", err, driver.ErrNotFound)
	if de := asDriverError(t, err); de.Code != "404" {
		t.Errorf("code = %q, want 404", de.Code)
	}
	_, err = c.Collections(ctx, driver.NamespaceRef{Name: "not/a/repo"})
	wantKind(t, "Collections of a name with two slashes", err, driver.ErrNotFound)
}

// Without a namespace the scope is the DSN's owner, else the token's login;
// without either there is nothing to resolve.
func TestEmptyNamespaceResolvesToTheOwner(t *testing.T) {
	f := newFakeGitHub(t)
	ctx := context.Background()
	org := f.openDSN(t, f.dsn("acme", ""), false, fakeToken)
	cols, err := org.Collections(ctx, driver.NamespaceRef{})
	if err != nil || len(cols) != len(ownerCollections) || cols[0].Ref.Namespace != "acme" {
		t.Errorf("Collections of an owner DSN without a namespace = %+v, %v", cols, err)
	}
	s, err := org.Describe(ctx, driver.CollectionRef{Name: "issues"})
	if err != nil || s.Ref.Namespace != "acme" || !slices.Equal(s.PrimaryKey, []string{"repository", "number"}) {
		t.Errorf("Describe without a namespace = %+v, %v", s, err)
	}
	mine := f.openDSN(t, f.dsn("", ""), false, fakeToken)
	cols, err = mine.Collections(ctx, driver.NamespaceRef{})
	if err != nil || len(cols) != len(ownerCollections) || cols[0].Ref.Namespace != "octo" {
		t.Errorf("Collections of a bare DSN = %+v, %v", cols, err)
	}
	_, rows, _ := queryWith(t, mine, driver.Query{Text: `{"collection":"issues","limit":5}`})
	if len(rows) != 5 || !slices.Contains(searchQueries(f, "/search/issues"), "user:octo is:issue") {
		t.Errorf("a list on a bare DSN: %d rows, searches %v", len(rows), searchQueries(f, "/search/issues"))
	}
	anonymous := f.openDSN(t, f.dsn("", ""), false, "")
	_, err = anonymous.Collections(ctx, driver.NamespaceRef{})
	wantKind(t, "Collections of a bare DSN without a token", err, driver.ErrSyntax)
	if !strings.Contains(err.Error(), "name the repository or owner") {
		t.Errorf("refusal = %v", err)
	}
	_, err = anonymous.Query(ctx, driver.Query{Text: `{"collection":"issues"}`})
	wantKind(t, "a list on a bare DSN without a token", err, driver.ErrSyntax)
}

// Describe of an owner's collections: issues and pull requests are keyed by
// repository and number, commits by repository and sha, repositories by
// full_name; every row carries the repository; commits and repositories
// are read-only, issues and pull requests are not (writes route to the
// repository).
func TestDescribeOwnerCollections(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	ctx := context.Background()
	tests := []struct {
		name     string
		key      []string
		readOnly bool
	}{
		{"repositories", []string{"full_name"}, true},
		{"issues", []string{"repository", "number"}, false},
		{"pull_requests", []string{"repository", "number"}, false},
		{"commits", []string{"repository", "sha"}, true},
	}
	for _, tc := range tests {
		s, err := c.Describe(ctx, driver.CollectionRef{Namespace: "acme", Name: tc.name})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if s.Ref != (driver.CollectionRef{Namespace: "acme", Name: tc.name}) || !slices.Equal(s.PrimaryKey, tc.key) || s.RowEstimate != -1 || s.SizeBytes != -1 {
			t.Errorf("%s: schema = %+v", tc.name, s)
		}
		if strings.Contains(s.Comment, "read-only") != tc.readOnly {
			t.Errorf("%s: comment %q, read-only %v", tc.name, s.Comment, tc.readOnly)
		}
		if tc.name != "repositories" {
			repo := fieldByName(t, s, "repository")
			if repo.Type != driver.TypeString || !repo.Generated || repo.NativeType != "owner/name" {
				t.Errorf("%s: repository = %+v", tc.name, repo)
			}
		}
	}
	// The issue columns are the repository collection's plus the
	// repository; a pull request through search has the issue view.
	issues, _ := c.Describe(ctx, driver.CollectionRef{Namespace: "acme", Name: "issues"})
	if len(issues.Fields) != len(issueColumns)+1 || issues.Fields[len(issues.Fields)-1].Name != "repository" {
		t.Errorf("owner issue fields = %d, want the issue columns and repository last", len(issues.Fields))
	}
	pulls, _ := c.Describe(ctx, driver.CollectionRef{Namespace: "acme", Name: "pull_requests"})
	if len(pulls.Fields) != len(issueColumns)+1 || !strings.Contains(pulls.Comment, "no head, base or merge data") {
		t.Errorf("owner pull request fields = %d, comment %q", len(pulls.Fields), pulls.Comment)
	}
	_, err := c.Describe(ctx, driver.CollectionRef{Namespace: "acme", Name: "labels"})
	wantKind(t, "a repository collection in an owner namespace", err, driver.ErrNotFound)
	if !strings.Contains(err.Error(), "the collections are repositories, issues, pull_requests, commits") {
		t.Errorf("refusal = %v", err)
	}
	_, err = c.Describe(ctx, driver.CollectionRef{Namespace: "nobody", Name: "issues"})
	wantKind(t, "Describe in an unknown owner", err, driver.ErrNotFound)
}

// Browse.

// Browsing an organization's issues goes through /search/issues with
// org:acme is:issue; the rows come from every repository, private ones
// included with a token, and carry their repository.
func TestBrowseOwnerIssuesThroughSearch(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.openDSN(t, f.dsn("acme", ""), false, fakeToken)
	f.resetLog()
	ref := driver.CollectionRef{Namespace: "acme", Name: "issues"}
	cols, rows, stats := browse(t, c, ref, driver.BrowseOptions{})
	if len(rows) != 5 || stats.Truncated || stats.NextCursor != "" {
		t.Fatalf("%d rows, stats %+v; want acme's five issues on one page", len(rows), stats)
	}
	reqs := f.requestsTo("/search/issues")
	if len(reqs) != 1 || reqs[0].Query.Get("q") != "org:acme is:issue" || reqs[0].Query.Get("per_page") != "100" || reqs[0].Query.Get("page") != "1" || reqs[0].Query.Get("sort") != "" {
		t.Errorf("search requests = %+v", reqs)
	}
	if len(f.requestsTo("/users/acme")) != 1 {
		t.Errorf("the owner was looked up %d times, want once", len(f.requestsTo("/users/acme")))
	}
	repos := repositoriesOf(t, cols, rows)
	slices.Sort(repos)
	if !slices.Equal(repos, []string{"acme/alpha", "acme/alpha", "acme/alpha", "acme/alpha", "acme/beta"}) {
		t.Errorf("repositories = %v", repos)
	}
	if last := cols[len(cols)-1]; last.Name != "repository" || last.Source == nil || last.Source.Namespace != "acme" || last.Source.Name != "issues" || !cols[0].Key {
		t.Errorf("columns = %+v", cols)
	}
	for _, row := range rows {
		if title, _ := cell(t, cols, row, "title").AsText(); !strings.Contains(title, "issue") {
			t.Errorf("title = %q", title)
		}
	}
	// The same through the pinned connection, naming the owner as the
	// namespace, and for a user with user:.
	pinned := f.open(t, false)
	f.resetLog()
	cols, rows, _ = browse(t, pinned, driver.CollectionRef{Namespace: "octo", Name: "issues"}, driver.BrowseOptions{Limit: 300})
	if len(rows) != 253 || !slices.Equal(searchQueries(f, "/search/issues"), []string{"user:octo is:issue", "user:octo is:issue", "user:octo is:issue"}) {
		t.Errorf("octo's issues: %d rows, searches %v", len(rows), searchQueries(f, "/search/issues"))
	}
	for _, row := range rows {
		if repo, _ := cell(t, cols, row, "repository").AsText(); repo != "octo/widgets" && repo != "octo/gadgets" {
			t.Errorf("repository = %q", repo)
		}
	}
}

// Pull requests of an owner are the is:pr side of the same search.
func TestBrowseOwnerPullRequestsThroughSearch(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.resetLog()
	cols, rows, stats := browse(t, c, driver.CollectionRef{Namespace: "octo", Name: "pull_requests"}, driver.BrowseOptions{})
	if len(rows) != 20 || stats.Truncated {
		t.Fatalf("%d rows, want the 20 pull requests of octo/widgets", len(rows))
	}
	if got := searchQueries(f, "/search/issues"); !slices.Equal(got, []string{"user:octo is:pr"}) {
		t.Errorf("searches = %v", got)
	}
	for _, row := range rows {
		n, _ := cell(t, cols, row, "number").AsInt()
		repo, _ := cell(t, cols, row, "repository").AsText()
		if n <= 250 || repo != "octo/widgets" {
			t.Errorf("row number=%d repository=%s is not a widgets pull request", n, repo)
		}
	}
	if len(cols) != len(issueColumns)+1 {
		t.Errorf("%d columns, want the issue view and the repository", len(cols))
	}
}

// Conditions the search can take travel as qualifiers and narrow the
// results; sorting is the search's sort and order; the rest is applied in
// tobira.
func TestBrowseOwnerIssuesPushesQualifiers(t *testing.T) {
	f := newFakeGitHub(t)
	seedAcmeVariety(f)
	c := f.openDSN(t, f.dsn("acme", ""), false, fakeToken)
	ref := driver.CollectionRef{Namespace: "acme", Name: "issues"}
	cond := func(field string, op driver.Op, v driver.Value) driver.Condition {
		return driver.Condition{Field: field, Op: op, Value: v}
	}
	tests := []struct {
		name  string
		conds []driver.Condition
		q     string
		rows  int
	}{
		{"state closed", []driver.Condition{cond("state", driver.OpEq, driver.String("closed"))}, "org:acme is:issue state:closed", 1},
		{"label", []driver.Condition{cond("labels", driver.OpContains, driver.String("bug"))}, "org:acme is:issue label:bug", 6},
		{"author", []driver.Condition{cond("user", driver.OpEq, driver.String("hubot"))}, "org:acme is:issue author:hubot", 1},
		{"assignee", []driver.Condition{cond("assignees", driver.OpEq, driver.String("hubot"))}, "org:acme is:issue assignee:hubot", 1},
		{"no assignee", []driver.Condition{cond("assignees", driver.OpIsNull, driver.Null())}, "org:acme is:issue no:assignee", 5},
		{"milestone", []driver.Condition{cond("milestone", driver.OpEq, driver.String("v1"))}, "org:acme is:issue milestone:v1", 1},
		{"no milestone", []driver.Condition{cond("milestone", driver.OpIsNull, driver.Null())}, "org:acme is:issue no:milestone", 5},
		{"comments", []driver.Condition{cond("comments", driver.OpGt, driver.Int(3))}, "org:acme is:issue comments:>3", 1},
		{"created since, none", []driver.Condition{cond("created_at", driver.OpGe, driver.Time(seedBase.Add(11*time.Hour)))}, "org:acme is:issue created:>=2024-01-01T11:00:00Z", 0},
		{"created since an hour", []driver.Condition{cond("created_at", driver.OpGe, driver.Time(seedBase.Add(5*time.Hour)))}, "org:acme is:issue created:>=2024-01-01T05:00:00Z", 1},
		{"updated before", []driver.Condition{cond("updated_at", driver.OpLt, driver.Time(seedBase.Add(2*time.Hour)))}, "org:acme is:issue updated:<2024-01-01T02:00:00Z", 2},
		{"closed after", []driver.Condition{cond("closed_at", driver.OpGt, driver.Time(seedBase))}, "org:acme is:issue closed:>2024-01-01T00:00:00Z", 1},
		{"one repository by full name", []driver.Condition{cond("repository", driver.OpEq, driver.String("acme/beta"))}, "is:issue repo:acme/beta", 1},
		{"one repository by name", []driver.Condition{cond("repository", driver.OpEq, driver.String("alpha"))}, "is:issue repo:acme/alpha", 5},
		{"several qualifiers", []driver.Condition{cond("state", driver.OpEq, driver.String("open")), cond("labels", driver.OpEq, driver.String("bug")), cond("user", driver.OpEq, driver.String("acme"))}, "org:acme is:issue state:open label:bug author:acme", 4},
		{"local title prefix beside a qualifier", []driver.Condition{cond("state", driver.OpEq, driver.String("open")), cond("title", driver.OpPrefix, driver.String("beta"))}, "org:acme is:issue state:open", 1},
		{"local state ne", []driver.Condition{cond("state", driver.OpNe, driver.String("open"))}, "org:acme is:issue", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f.resetLog()
			_, rows, _ := browse(t, c, ref, driver.BrowseOptions{Filter: driver.Filter{Conditions: tc.conds}})
			if got := searchQueries(f, "/search/issues"); !slices.Equal(got, []string{tc.q}) {
				t.Errorf("searches = %v, want [%q]", got, tc.q)
			}
			if len(rows) != tc.rows {
				t.Errorf("%d rows, want %d", len(rows), tc.rows)
			}
		})
	}
	t.Run("sorting", func(t *testing.T) {
		f.resetLog()
		cols, rows, _ := browse(t, c, ref, driver.BrowseOptions{Sort: []driver.SortKey{{Field: "comments", Desc: true}}})
		reqs := f.requestsTo("/search/issues")
		if len(reqs) != 1 || reqs[0].Query.Get("sort") != "comments" || reqs[0].Query.Get("order") != "desc" {
			t.Errorf("requests = %+v", reqs)
		}
		if n, _ := cell(t, cols, rows[0], "comments").AsInt(); n != 5 {
			t.Errorf("the most commented issue comes first: comments = %d", n)
		}
		f.resetLog()
		cols, rows, _ = browse(t, c, ref, driver.BrowseOptions{Sort: []driver.SortKey{{Field: "created_at"}}})
		reqs = f.requestsTo("/search/issues")
		if len(reqs) != 1 || reqs[0].Query.Get("sort") != "created" || reqs[0].Query.Get("order") != "asc" {
			t.Errorf("requests = %+v", reqs)
		}
		first, _ := cell(t, cols, rows[0], "created_at").AsTime()
		last, _ := cell(t, cols, rows[len(rows)-1], "created_at").AsTime()
		if !first.Before(last) {
			t.Errorf("ascending by created_at gave %v before %v", first, last)
		}
		_, err := c.Browse(context.Background(), ref, driver.BrowseOptions{Sort: []driver.SortKey{{Field: "title"}}})
		wantKind(t, "sorting by title", err, driver.ErrUnsupported)
		if !strings.Contains(err.Error(), `cannot sort issues by "title": it sorts by comments, created_at, updated_at`) {
			t.Errorf("refusal = %v", err)
		}
	})
	t.Run("unknown field", func(t *testing.T) {
		_, err := c.Browse(context.Background(), ref, driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{cond("head", driver.OpEq, driver.String("main"))}}})
		wantKind(t, "a field of the repository view only", err, driver.ErrSyntax)
		if !strings.Contains(err.Error(), `unknown field "head" in issues`) {
			t.Errorf("refusal = %v", err)
		}
	})
}

// planBrowse renders every qualifier the owner-wide collections take, and
// leaves the rest local; the commit search needs free text.
func TestPlanBrowseOwnerQualifiers(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	ctx := context.Background()
	t1 := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	cond := func(field string, op driver.Op, v driver.Value) driver.Condition {
		return driver.Condition{Field: field, Op: op, Value: v}
	}
	plan := func(t *testing.T, owner, col string, opts driver.BrowseOptions) (listRequest, []driver.Condition) {
		t.Helper()
		req, local, err := c.planBrowse(ctx, "browse", ownerCollectionByName(col), scope{owner: owner}, opts, 100)
		if err != nil {
			t.Fatalf("planBrowse: %v", err)
		}
		return req, local
	}
	filter := func(conds ...driver.Condition) driver.BrowseOptions {
		return driver.BrowseOptions{Filter: driver.Filter{Conditions: conds}}
	}
	t.Run("issue qualifiers", func(t *testing.T) {
		tests := []struct {
			name  string
			conds []driver.Condition
			q     string
			local int
		}{
			{"labels eq", []driver.Condition{cond("labels", driver.OpEq, driver.String("bug"))}, "org:acme is:issue label:bug", 0},
			{"labels is_null", []driver.Condition{cond("labels", driver.OpIsNull, driver.Null())}, "org:acme is:issue no:label", 0},
			{"label with a space is quoted", []driver.Condition{cond("labels", driver.OpContains, driver.String("good first issue"))}, `org:acme is:issue label:"good first issue"`, 0},
			{"assignees contains", []driver.Condition{cond("assignees", driver.OpContains, driver.String("octo"))}, "org:acme is:issue assignee:octo", 0},
			{"assignees not_null stays local", []driver.Condition{cond("assignees", driver.OpNotNull, driver.Null())}, "org:acme is:issue", 1},
			{"milestone with a space is quoted", []driver.Condition{cond("milestone", driver.OpEq, driver.String("v1 beta"))}, `org:acme is:issue milestone:"v1 beta"`, 0},
			{"created eq", []driver.Condition{cond("created_at", driver.OpEq, driver.Time(t1))}, "org:acme is:issue created:2024-03-04T05:06:07Z", 0},
			{"created as text", []driver.Condition{cond("created_at", driver.OpGe, driver.String("2024-01-01"))}, "org:acme is:issue created:>=2024-01-01", 0},
			{"updated le", []driver.Condition{cond("updated_at", driver.OpLe, driver.Time(t1))}, "org:acme is:issue updated:<=2024-03-04T05:06:07Z", 0},
			{"closed lt", []driver.Condition{cond("closed_at", driver.OpLt, driver.Time(t1))}, "org:acme is:issue closed:<2024-03-04T05:06:07Z", 0},
			{"comments ge", []driver.Condition{cond("comments", driver.OpGe, driver.Int(1))}, "org:acme is:issue comments:>=1", 0},
			{"comments eq", []driver.Condition{cond("comments", driver.OpEq, driver.Int(0))}, "org:acme is:issue comments:0", 0},
			{"comments ne stays local", []driver.Condition{cond("comments", driver.OpNe, driver.Int(0))}, "org:acme is:issue", 1},
			{"created in stays local", []driver.Condition{cond("created_at", driver.OpIn, driver.List(driver.Time(t1)))}, "org:acme is:issue", 1},
			{"empty value stays local", []driver.Condition{cond("state", driver.OpEq, driver.String(""))}, "org:acme is:issue", 1},
			{"repository ne stays local", []driver.Condition{cond("repository", driver.OpNe, driver.String("alpha"))}, "org:acme is:issue", 1},
			{"repository replaces the owner", []driver.Condition{cond("repository", driver.OpEq, driver.String("alpha")), cond("state", driver.OpEq, driver.String("open"))}, "is:issue repo:acme/alpha state:open", 0},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				req, local := plan(t, "acme", "issues", filter(tc.conds...))
				if got := req.query.Get("q"); !sameTerms(got, tc.q) {
					t.Errorf("q = %q, want %q", got, tc.q)
				}
				if len(local) != tc.local {
					t.Errorf("local = %+v, want %d conditions", local, tc.local)
				}
				if req.path() != "/search/issues" || req.envelope() != "items" || req.perPage != 100 || req.repo != "" || req.owner != "acme" {
					t.Errorf("request = %+v", req)
				}
			})
		}
		req, _ := plan(t, "octo", "pull_requests", filter(cond("state", driver.OpEq, driver.String("closed"))))
		if got := req.query.Get("q"); got != "user:octo is:pr state:closed" {
			t.Errorf("a user's pull requests: q = %q", got)
		}
	})
	t.Run("commit qualifiers", func(t *testing.T) {
		message := cond("message", driver.OpContains, driver.String("fix"))
		tests := []struct {
			name  string
			conds []driver.Condition
			q     string
			local int
		}{
			{"message alone", []driver.Condition{message}, "fix user:octo", 1},
			{"phrase is quoted", []driver.Condition{cond("message", driver.OpContains, driver.String("fix a typo"))}, `"fix a typo" user:octo`, 1},
			{"author", []driver.Condition{message, cond("author", driver.OpEq, driver.String("octo"))}, "fix user:octo author:octo", 1},
			{"committer", []driver.Condition{message, cond("committer", driver.OpEq, driver.String("web-flow"))}, "fix user:octo committer:web-flow", 1},
			{"author name", []driver.Condition{message, cond("author_name", driver.OpEq, driver.String("Octo Cat"))}, `fix user:octo author-name:"Octo Cat"`, 1},
			{"author email", []driver.Condition{message, cond("author_email", driver.OpEq, driver.String("octo@example.com"))}, "fix user:octo author-email:octo@example.com", 1},
			{"author date", []driver.Condition{message, cond("author_date", driver.OpGe, driver.Time(t1))}, "fix user:octo author-date:>=2024-03-04T05:06:07Z", 1},
			{"committer date", []driver.Condition{message, cond("committer_date", driver.OpLe, driver.Time(t1))}, "fix user:octo committer-date:<=2024-03-04T05:06:07Z", 1},
			{"repository", []driver.Condition{message, cond("repository", driver.OpEq, driver.String("widgets"))}, "fix repo:octo/widgets", 1},
			{"committer name stays local", []driver.Condition{message, cond("committer_name", driver.OpEq, driver.String("GitHub"))}, "fix user:octo", 2},
			{"message prefix stays local", []driver.Condition{message, cond("message", driver.OpPrefix, driver.String("Commit"))}, "fix user:octo", 2},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				req, local := plan(t, "octo", "commits", filter(tc.conds...))
				if got := req.query.Get("q"); !sameTerms(got, tc.q) {
					t.Errorf("q = %q, want %q", got, tc.q)
				}
				if len(local) != tc.local {
					t.Errorf("local = %+v, want %d conditions", local, tc.local)
				}
				if req.path() != "/search/commits" {
					t.Errorf("path = %q", req.path())
				}
			})
		}
		for name, opts := range map[string]driver.BrowseOptions{
			"no filter":             {},
			"qualifiers only":       filter(cond("author", driver.OpEq, driver.String("octo"))),
			"blank message":         filter(cond("message", driver.OpContains, driver.String("  "))),
			"message prefix only":   filter(cond("message", driver.OpPrefix, driver.String("fix"))),
			"message eq is no text": filter(cond("message", driver.OpEq, driver.String("fix"))),
		} {
			_, _, err := c.planBrowse(ctx, "browse", ownerCollectionByName("commits"), scope{owner: "octo"}, opts, 100)
			wantKind(t, "commits "+name, err, driver.ErrUnsupported)
			if !strings.Contains(err.Error(), "commit search needs text") || !strings.Contains(err.Error(), "message contains") {
				t.Errorf("%s: refusal = %v", name, err)
			}
		}
	})
	t.Run("sorting", func(t *testing.T) {
		for _, tc := range []struct {
			col, field, sort, order string
			desc                    bool
		}{
			{"issues", "created_at", "created", "desc", true},
			{"issues", "updated_at", "updated", "asc", false},
			{"pull_requests", "comments", "comments", "desc", true},
			{"commits", "author_date", "author-date", "desc", true},
			{"commits", "committer_date", "committer-date", "asc", false},
		} {
			opts := driver.BrowseOptions{Sort: []driver.SortKey{{Field: tc.field, Desc: tc.desc}}}
			if tc.col == "commits" {
				opts.Filter.Conditions = []driver.Condition{cond("message", driver.OpContains, driver.String("fix"))}
			}
			req, _ := plan(t, "acme", tc.col, opts)
			if req.query.Get("sort") != tc.sort || req.query.Get("order") != tc.order || req.query.Get("direction") != "" {
				t.Errorf("%s by %s: query %v", tc.col, tc.field, req.query)
			}
		}
		_, _, err := c.planBrowse(ctx, "browse", ownerCollectionByName("commits"), scope{owner: "acme"}, driver.BrowseOptions{Filter: filter(cond("message", driver.OpContains, driver.String("x"))).Filter, Sort: []driver.SortKey{{Field: "message"}}}, 100)
		wantKind(t, "sorting commits by message", err, driver.ErrUnsupported)
		if !strings.Contains(err.Error(), "it sorts by author_date, committer_date") {
			t.Errorf("refusal = %v", err)
		}
		_, _, err = c.planBrowse(ctx, "browse", ownerCollectionByName("issues"), scope{owner: "acme"}, driver.BrowseOptions{Sort: []driver.SortKey{{Field: "created_at"}, {Field: "comments"}}}, 100)
		wantKind(t, "two sort keys", err, driver.ErrUnsupported)
		if got := sortableHint(ownerCollectionByName("pull_requests")); got != ": it sorts by comments, created_at, updated_at" {
			t.Errorf("sortableHint = %q", got)
		}
	})
	t.Run("repositories", func(t *testing.T) {
		for owner, want := range map[string]struct {
			path  string
			query url.Values
		}{
			"acme":  {"/orgs/acme/repos", url.Values{"type": {"all"}, "sort": {"full_name"}}},
			"hubot": {"/users/hubot/repos", url.Values{"type": {"owner"}, "sort": {"full_name"}}},
			"octo":  {"/user/repos", url.Values{"affiliation": {"owner"}, "sort": {"full_name"}}},
		} {
			req, local := plan(t, owner, "repositories", filter(cond("language", driver.OpEq, driver.String("Go"))))
			if req.path() != want.path || !reflect.DeepEqual(req.query, want.query) || req.envelope() != "" || len(local) != 1 {
				t.Errorf("%s: path %q, query %v, local %+v", owner, req.path(), req.query, local)
			}
		}
		req, _ := plan(t, "acme", "repositories", driver.BrowseOptions{Sort: []driver.SortKey{{Field: "pushed_at", Desc: true}}})
		if want := (url.Values{"type": {"all"}, "sort": {"pushed"}, "direction": {"desc"}}); !reflect.DeepEqual(req.query, want) {
			t.Errorf("sorted listing = %v, want %v", req.query, want)
		}
		req, _ = plan(t, "acme", "repositories", driver.BrowseOptions{Sort: []driver.SortKey{{Field: "full_name"}}})
		if req.query.Get("sort") != "full_name" || req.query.Get("direction") != "asc" {
			t.Errorf("sorted by full_name = %v", req.query)
		}
		_, _, err := c.planBrowse(ctx, "browse", ownerCollectionByName("repositories"), scope{owner: "acme"}, driver.BrowseOptions{Sort: []driver.SortKey{{Field: "stars"}}}, 100)
		wantKind(t, "sorting repositories by stars", err, driver.ErrUnsupported)
		if !strings.Contains(err.Error(), "it sorts by created_at, full_name, pushed_at, updated_at") {
			t.Errorf("refusal = %v", err)
		}
	})
	t.Run("hash tells scopes and qualifiers apart", func(t *testing.T) {
		a, _ := plan(t, "acme", "issues", filter(cond("state", driver.OpEq, driver.String("open"))))
		b, _ := plan(t, "acme", "issues", filter(cond("state", driver.OpEq, driver.String("closed"))))
		d, _ := plan(t, "acme", "pull_requests", filter(cond("state", driver.OpEq, driver.String("open"))))
		e, _ := plan(t, "octo", "issues", filter(cond("state", driver.OpEq, driver.String("open"))))
		again, _ := plan(t, "acme", "issues", filter(cond("state", driver.OpEq, driver.String("open"))))
		if a.hash() != again.hash() || a.hash() == b.hash() || a.hash() == d.hash() || a.hash() == e.hash() {
			t.Errorf("hashes: %s %s %s %s %s", a.hash(), again.hash(), b.hash(), d.hash(), e.hash())
		}
	})
}

// A page of owner issues ends with a cursor that resumes on the next search
// page, exactly as for a repository; a cursor of another filter is refused.
func TestBrowseOwnerIssuesPagesWithACursor(t *testing.T) {
	f := newFakeGitHub(t)
	seedIssues(f, "acme", "alpha", 150)
	c := f.openDSN(t, f.dsn("acme", ""), false, fakeToken)
	ref := driver.CollectionRef{Namespace: "acme", Name: "issues"}
	f.resetLog()
	cols, first, stats := browse(t, c, ref, driver.BrowseOptions{})
	if len(first) != 100 || !stats.Truncated || stats.NextCursor == "" {
		t.Fatalf("first page: %d rows, stats %+v", len(first), stats)
	}
	if pages := pagesRequested(f.requestsTo("/search/issues")); !reflect.DeepEqual(pages, []int{1}) {
		t.Errorf("first page requested search pages %v", pages)
	}
	f.resetLog()
	_, second, stats := browse(t, c, ref, driver.BrowseOptions{Cursor: stats.NextCursor})
	if len(second) != 55 || stats.Truncated || stats.NextCursor != "" {
		t.Fatalf("second page: %d rows, stats %+v", len(second), stats)
	}
	if pages := pagesRequested(f.requestsTo("/search/issues")); !reflect.DeepEqual(pages, []int{2}) {
		t.Errorf("second page requested search pages %v", pages)
	}
	seen := map[string]bool{}
	for _, row := range slices.Concat(first, second) {
		repo, _ := cell(t, cols, row, "repository").AsText()
		n, _ := cell(t, cols, row, "number").AsInt()
		key := fmt.Sprintf("%s#%d", repo, n)
		if seen[key] {
			t.Errorf("%s appears twice", key)
		}
		seen[key] = true
	}
	if len(seen) != 155 {
		t.Errorf("%d distinct issues, want 155", len(seen))
	}
	// Smaller pages are the search's per_page, and a filtered page that
	// stops mid-way resumes from the item after the last one shown.
	f.resetLog()
	_, small, stats := browse(t, c, ref, driver.BrowseOptions{Limit: 30, Filter: driver.Filter{Conditions: []driver.Condition{{Field: "title", Op: driver.OpPrefix, Value: driver.String("alpha extra")}}}})
	if len(small) != 30 || !stats.Truncated || !strings.HasPrefix(stats.NextCursor, "p|") {
		t.Fatalf("small page: %d rows, stats %+v", len(small), stats)
	}
	if reqs := f.requestsTo("/search/issues"); len(reqs) == 0 || reqs[0].Query.Get("per_page") != "30" {
		t.Errorf("small page requests = %+v", reqs)
	}
	_, err := c.Browse(context.Background(), ref, driver.BrowseOptions{Cursor: stats.NextCursor})
	wantKind(t, "the cursor of a filtered browse without its filter", err, driver.ErrSyntax)
	if !strings.Contains(err.Error(), "other filters, sorting or page size") {
		t.Errorf("refusal = %v", err)
	}
	_, err = c.Browse(context.Background(), driver.CollectionRef{Namespace: "acme", Name: "pull_requests"}, driver.BrowseOptions{Cursor: stats.NextCursor})
	wantKind(t, "the cursor of another collection", err, driver.ErrSyntax)
}

// syntheticSearchPages answers /search/issues with pages of perPage issues
// without comments, so a filter on comments scans them all, over the given
// number of pages.
func syntheticSearchPages(pages, perPage int) func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
	return func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path != "/search/issues" || r.Method != http.MethodGet {
			return false
		}
		page := atoiDefault(r.URL.Query().Get("page"), 1)
		items := make([]object, 0, perPage)
		for i := 0; i < perPage; i++ {
			n := (page-1)*perPage + i + 1
			items = append(items, object{"number": n, "title": fmt.Sprintf("Synthetic issue %d", n), "state": "open", "user": userObj("acme"),
				"labels": []object{}, "assignees": []object{}, "comments": 0, "created_at": at(0), "updated_at": at(0),
				"repository_url": "https://api.github.com/repos/acme/alpha", "score": 1.0})
		}
		if link := linkHeader(r, page, perPage, pages*perPage); link != "" {
			w.Header().Set("Link", link)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		body, _ := json.Marshal(object{"total_count": pages * perPage, "incomplete_results": false, "items": items})
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return true
	}
}

// When the search pages are thin and a local filter matches nothing, the
// request cap ends the page before the item cap; the warning says so and
// the cursor resumes after the last page read.
func TestBrowseOwnerIssuesStopsAfterTenRequests(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.openDSN(t, f.dsn("acme", ""), false, fakeToken)
	ref := driver.CollectionRef{Namespace: "acme", Name: "issues"}
	f.setHook(syntheticSearchPages(12, 50))
	f.resetLog()
	opts := driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "comments", Op: driver.OpNe, Value: driver.Int(0)}}}}
	cols, rows, stats := browse(t, c, ref, opts)
	searches := f.requestsTo("/search/issues")
	if len(rows) != 0 || !stats.Truncated || stats.NextCursor == "" || len(searches) != maxRequests {
		t.Fatalf("%d rows, stats %+v, %d searches", len(rows), stats, len(searches))
	}
	if len(stats.Warnings) != 1 || !strings.Contains(stats.Warnings[0], "stopped after 10 requests") {
		t.Errorf("warnings = %v", stats.Warnings)
	}
	if got := searchQueries(f, "/search/issues"); got[0] != "org:acme is:issue" {
		t.Errorf("searches = %v", got)
	}
	if last := cols[len(cols)-1]; last.Name != "repository" {
		t.Errorf("columns = %v", columnNames(cols))
	}
	f.resetLog()
	opts.Cursor = stats.NextCursor
	_, rows, stats = browse(t, c, ref, opts)
	if pages := pagesRequested(f.requestsTo("/search/issues")); !reflect.DeepEqual(pages, []int{11, 12}) || stats.Truncated || stats.NextCursor != "" || len(rows) != 0 {
		t.Errorf("continued page: pages %v, %d rows, stats %+v", pages, len(rows), stats)
	}
	// The same cursor with the hook gone is still read page by page from
	// the fake's own search, which refuses nothing.
	f.setHook(nil)
	if _, err := c.Browse(context.Background(), ref, opts); err != nil {
		t.Errorf("resuming on the real listing: %v", err)
	}
}

// An owner's repositories are listed through the owner's listing endpoint
// (an organization's under /orgs, a user's under /users, the token's own
// under /user), sorted by the listing, the other conditions applied in
// tobira.
func TestBrowseOwnerRepositories(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.resetLog()
	cols, rows, stats := browse(t, c, driver.CollectionRef{Namespace: "acme", Name: "repositories"}, driver.BrowseOptions{})
	if len(rows) != 2 || stats.Truncated {
		t.Fatalf("%d rows, stats %+v; want alpha and beta", len(rows), stats)
	}
	reqs := f.requestsTo("/orgs/acme/repos")
	if len(reqs) != 1 || reqs[0].Query.Get("type") != "all" || reqs[0].Query.Get("sort") != "full_name" || reqs[0].Query.Get("per_page") != "100" || reqs[0].Query.Get("page") != "1" {
		t.Errorf("listing requests = %+v", reqs)
	}
	if len(f.requestsTo("/search/repositories")) != 0 || len(f.requestsTo("/users/acme/repos")) != 0 {
		t.Errorf("the listing went elsewhere: %+v", f.log())
	}
	names := make([]string, len(rows))
	for i, row := range rows {
		names[i], _ = cell(t, cols, row, "full_name").AsText()
	}
	if !slices.Equal(names, []string{"acme/alpha", "acme/beta"}) {
		t.Errorf("full_name = %v", names)
	}
	if !cols[0].Key || cols[0].Name != "full_name" || cols[0].Source == nil || cols[0].Source.Namespace != "acme" || cols[0].Source.Name != "repositories" {
		t.Errorf("columns = %+v", cols[0])
	}
	if private, _ := cell(t, cols, rows[1], "private").AsBool(); !private {
		t.Error("beta, private, is missing its flag")
	}
	// Sorting is the listing's sort and direction; a filter the listing
	// cannot take is applied here.
	f.resetLog()
	cols, rows, _ = browse(t, c, driver.CollectionRef{Namespace: "acme", Name: "repositories"}, driver.BrowseOptions{
		Sort:   []driver.SortKey{{Field: "updated_at", Desc: true}},
		Filter: driver.Filter{Conditions: []driver.Condition{{Field: "language", Op: driver.OpEq, Value: driver.String("Python")}}},
	})
	reqs = f.requestsTo("/orgs/acme/repos")
	if len(reqs) != 1 || reqs[0].Query.Get("sort") != "updated" || reqs[0].Query.Get("direction") != "desc" || reqs[0].Query.Get("language") != "" {
		t.Errorf("sorted listing requests = %+v", reqs)
	}
	if len(rows) != 1 {
		t.Fatalf("%d rows, want beta alone", len(rows))
	}
	if name, _ := cell(t, cols, rows[0], "name").AsText(); name != "beta" {
		t.Errorf("name = %q", name)
	}
	// A user's and the token's own.
	f.resetLog()
	_, rows, _ = browse(t, c, driver.CollectionRef{Namespace: "hubot", Name: "repositories"}, driver.BrowseOptions{})
	if len(rows) != 1 || len(f.requestsTo("/users/hubot/repos")) != 1 || f.requestsTo("/users/hubot/repos")[0].Query.Get("type") != "owner" {
		t.Errorf("hubot's repositories: %d rows, %+v", len(rows), f.log())
	}
	f.resetLog()
	_, rows, _ = browse(t, c, driver.CollectionRef{Namespace: "octo", Name: "repositories"}, driver.BrowseOptions{Limit: 1})
	own := f.requestsTo("/user/repos")
	if len(rows) != 1 || len(own) != 1 || own[0].Query.Get("affiliation") != "owner" || own[0].Query.Get("per_page") != "1" {
		t.Errorf("octo's own repositories: %d rows, %+v", len(rows), own)
	}
	_, err := c.Browse(context.Background(), driver.CollectionRef{Namespace: "acme", Name: "repositories"}, driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "state", Op: driver.OpEq, Value: driver.String("open")}}}})
	wantKind(t, "an unknown repository field", err, driver.ErrSyntax)
}

// An owner's commits go through /search/commits, which needs text: a
// message condition gives it; the qualifiers narrow the authors and dates
// and the rows carry the repository.
func TestBrowseOwnerCommitsThroughSearch(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	ref := driver.CollectionRef{Namespace: "octo", Name: "commits"}
	message := driver.Condition{Field: "message", Op: driver.OpContains, Value: driver.String("typo")}
	f.resetLog()
	cols, rows, stats := browse(t, c, ref, driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{message}}})
	if len(rows) != 15 || stats.Truncated {
		t.Fatalf("%d rows, want the 15 typo fixes", len(rows))
	}
	if got := searchQueries(f, "/search/commits"); !slices.Equal(got, []string{"typo user:octo"}) {
		t.Errorf("searches = %v", got)
	}
	for _, row := range rows {
		repo, _ := cell(t, cols, row, "repository").AsText()
		msg, _ := cell(t, cols, row, "message").AsText()
		if repo != "octo/widgets" || !strings.Contains(msg, "typo") || cell(t, cols, row, "author_date").Type() != driver.TypeTime {
			t.Errorf("row repository=%s message=%q", repo, msg)
		}
	}
	if !cols[0].Key || cols[0].Name != "sha" || cols[len(cols)-1].Name != "repository" {
		t.Errorf("columns = %v", columnNames(cols))
	}
	// Author and date qualifiers, and the search's sort.
	f.resetLog()
	cols, rows, _ = browse(t, c, ref, driver.BrowseOptions{
		Filter: driver.Filter{Conditions: []driver.Condition{message, {Field: "author", Op: driver.OpEq, Value: driver.String("octo")}, {Field: "author_date", Op: driver.OpGe, Value: driver.Time(seedBase.Add(48 * time.Hour))}}},
		Sort:   []driver.SortKey{{Field: "author_date"}},
	})
	reqs := f.requestsTo("/search/commits")
	if len(reqs) != 1 || reqs[0].Query.Get("q") != "typo user:octo author:octo author-date:>=2024-01-03T00:00:00Z" || reqs[0].Query.Get("sort") != "author-date" || reqs[0].Query.Get("order") != "asc" {
		t.Errorf("requests = %+v", reqs)
	}
	// Typo fixes are every fourth commit and the ones without a matching
	// account every third; of the authored fixes, six are two days in or
	// later.
	if len(rows) != 6 {
		t.Errorf("%d rows, want 6", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		a, _ := cell(t, cols, rows[i-1], "author_date").AsTime()
		b, _ := cell(t, cols, rows[i], "author_date").AsTime()
		if a.After(b) {
			t.Errorf("ascending by author_date gave %v before %v", a, b)
		}
	}
	// An organization's, and one of its repositories.
	f.resetLog()
	cols, rows, _ = browse(t, c, driver.CollectionRef{Namespace: "acme", Name: "commits"}, driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "message", Op: driver.OpContains, Value: driver.String("Initial")}}}})
	repos := repositoriesOf(t, cols, rows)
	slices.Sort(repos)
	if !slices.Equal(repos, []string{"acme/alpha", "acme/beta"}) || !slices.Equal(searchQueries(f, "/search/commits"), []string{"Initial org:acme"}) {
		t.Errorf("acme's commits: %v, searches %v", repos, searchQueries(f, "/search/commits"))
	}
	f.resetLog()
	_, rows, _ = browse(t, c, driver.CollectionRef{Namespace: "acme", Name: "commits"}, driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "message", Op: driver.OpContains, Value: driver.String("Initial")}, {Field: "repository", Op: driver.OpEq, Value: driver.String("beta")}}}})
	if searches := searchQueries(f, "/search/commits"); len(rows) != 1 || len(searches) != 1 || !sameTerms(searches[0], "Initial repo:acme/beta") {
		t.Errorf("beta's commits: %d rows, searches %v", len(rows), searches)
	}
	// Without text the search cannot run, and nothing is sent.
	f.resetLog()
	_, err := c.Browse(context.Background(), ref, driver.BrowseOptions{})
	wantKind(t, "commits without a message condition", err, driver.ErrUnsupported)
	if len(f.requestsTo("/search/commits")) != 0 {
		t.Errorf("a refused browse searched: %+v", f.log())
	}
}

// An owner that does not exist, or a collection an owner does not have, is
// ErrNotFound before any search; a closed connection refuses.
func TestBrowseOwnerNotFound(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	ctx := context.Background()
	f.resetLog()
	_, err := c.Browse(ctx, driver.CollectionRef{Namespace: "nobody", Name: "issues"}, driver.BrowseOptions{})
	wantKind(t, "an unknown owner", err, driver.ErrNotFound)
	if de := asDriverError(t, err); de.Code != "404" || de.Op != "browse" {
		t.Errorf("attribution = %+v", de)
	}
	_, err = c.Browse(ctx, driver.CollectionRef{Namespace: "acme", Name: "labels"}, driver.BrowseOptions{})
	wantKind(t, "a repository collection of an owner", err, driver.ErrNotFound)
	if !strings.Contains(err.Error(), `collection "labels" does not exist in acme`) {
		t.Errorf("refusal = %v", err)
	}
	_, err = c.Browse(ctx, driver.CollectionRef{Namespace: "acme/alpha", Name: "repositories"}, driver.BrowseOptions{})
	wantKind(t, "an owner collection of a repository", err, driver.ErrNotFound)
	if n := len(f.requestsTo("/search/issues")); n != 0 {
		t.Errorf("a refused browse searched %d times", n)
	}
	closed := f.open(t, false)
	_ = closed.Close()
	_, err = closed.Browse(ctx, driver.CollectionRef{Namespace: "acme", Name: "issues"}, driver.BrowseOptions{})
	wantKind(t, "a closed connection", err, driver.ErrConnection)
}

// Query.

// A list in an owner scope takes q (appended to the search), sort and
// order, not the repository endpoint's parameters; repositories take the
// listing's type, sort and direction.
func TestListInAnOwnerScope(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.openDSN(t, f.dsn("acme", ""), false, fakeToken)
	ctx := context.Background()
	f.resetLog()
	cols, rows, stats := queryText(t, c, `{"collection":"issues"}`)
	if len(rows) != 5 || stats.Truncated || cols[0].Source == nil || cols[0].Source.Namespace != "acme" || cols[len(cols)-1].Name != "repository" {
		t.Fatalf("%d rows, stats %+v, columns %v from %+v", len(rows), stats, columnNames(cols), cols[0].Source)
	}
	reqs := f.requestsTo("/search/issues")
	if len(reqs) != 1 || reqs[0].Query.Get("q") != "org:acme is:issue" || reqs[0].Query.Get("per_page") != "100" || reqs[0].Query.Get("page") != "1" {
		t.Errorf("requests = %+v", reqs)
	}
	f.resetLog()
	_, rows, _ = queryText(t, c, `{"collection":"issues","q":"label:bug author:acme","sort":"created","order":"asc","limit":3}`)
	reqs = f.requestsTo("/search/issues")
	if len(rows) != 3 || len(reqs) != 1 || reqs[0].Query.Get("q") != "label:bug author:acme org:acme is:issue" || reqs[0].Query.Get("sort") != "created" || reqs[0].Query.Get("order") != "asc" || reqs[0].Query.Get("per_page") != "3" {
		t.Errorf("%d rows, requests = %+v", len(rows), reqs)
	}
	// A repo: qualifier in q replaces the owner, as GitHub ORs them.
	f.resetLog()
	_, rows, _ = queryText(t, c, `{"collection":"issues","q":"repo:acme/beta"}`)
	if searches := searchQueries(f, "/search/issues"); len(rows) != 1 || len(searches) != 1 || !slices.Equal(strings.Fields(searches[0]), []string{"repo:acme/beta", "is:issue"}) {
		t.Errorf("repo: in q: %d rows, searches %v", len(rows), searches)
	}
	f.resetLog()
	_, rows, _ = queryWith(t, c, driver.Query{Text: `{"collection":"pull_requests"}`, Namespace: "octo"})
	if len(rows) != 20 || !slices.Equal(searchQueries(f, "/search/issues"), []string{"user:octo is:pr"}) {
		t.Errorf("octo's pull requests through the namespace: %d rows, searches %v", len(rows), searchQueries(f, "/search/issues"))
	}
	// Repository parameters are refused in an owner scope, before any
	// request.
	f.resetLog()
	_, err := c.Query(ctx, driver.Query{Text: `{"collection":"issues","state":"open"}`})
	wantKind(t, "state in an owner scope", err, driver.ErrSyntax)
	if !strings.Contains(err.Error(), `"state" is not a parameter of issues in acme (q, sort, order)`) {
		t.Errorf("refusal = %v", err)
	}
	_, err = c.Query(ctx, driver.Query{Text: `{"collection":"issues","repo":"acme","labels":"bug"}`})
	wantKind(t, "labels in an owner scope", err, driver.ErrSyntax)
	_, err = c.Query(ctx, driver.Query{Text: `{"collection":"commits","q":"x"}`, Namespace: "acme/alpha"})
	wantKind(t, "q in a repository scope", err, driver.ErrSyntax)
	if !strings.Contains(err.Error(), `"q" is not a parameter of commits in acme/alpha`) {
		t.Errorf("refusal = %v", err)
	}
	if n := len(f.log()); n != 0 {
		t.Errorf("refused lists sent %d requests: %+v", n, f.log())
	}
	// Repositories.
	f.resetLog()
	cols, rows, _ = queryText(t, c, `{"collection":"repositories","type":"all","sort":"created","direction":"asc"}`)
	reqs = f.requestsTo("/orgs/acme/repos")
	if len(rows) != 2 || len(reqs) != 1 || reqs[0].Query.Get("type") != "all" || reqs[0].Query.Get("sort") != "created" || reqs[0].Query.Get("direction") != "asc" || !cols[0].Key || cols[0].Name != "full_name" {
		t.Errorf("repositories: %d rows, requests %+v", len(rows), reqs)
	}
	_, err = c.Query(ctx, driver.Query{Text: `{"collection":"repositories","q":"x"}`})
	wantKind(t, "q on repositories", err, driver.ErrSyntax)
	// Commits need text in q.
	f.resetLog()
	cols, rows, _ = queryText(t, c, `{"collection":"commits","q":"Initial","repo":"octo"}`)
	if len(rows) != 1 || !slices.Equal(searchQueries(f, "/search/commits"), []string{"Initial user:octo"}) {
		t.Errorf("octo's initial commits: %d rows, searches %v", len(rows), searchQueries(f, "/search/commits"))
	}
	if repo, _ := cell(t, cols, rows[0], "repository").AsText(); repo != "octo/gadgets" {
		t.Errorf("repository = %q", repo)
	}
	_, err = c.Query(ctx, driver.Query{Text: `{"collection":"commits"}`})
	wantKind(t, "commits without text", err, driver.ErrUnsupported)
	_, err = c.Query(ctx, driver.Query{Text: `{"collection":"commits","q":"author:acme"}`})
	wantKind(t, "commits with qualifiers only", err, driver.ErrUnsupported)
	if !strings.Contains(err.Error(), "commit search needs text") {
		t.Errorf("refusal = %v", err)
	}
	// The request cap and truncation work as for a repository listing.
	seedIssues(f, "acme", "alpha", 120)
	_, rows, stats = queryText(t, c, `{"collection":"issues","limit":1000}`)
	if len(rows) != 125 || stats.Truncated {
		t.Errorf("all issues: %d rows, stats %+v", len(rows), stats)
	}
	_, rows, stats = queryText(t, c, `{"collection":"issues","limit":50}`)
	if len(rows) != 50 || !stats.Truncated {
		t.Errorf("50 issues: %d rows, stats %+v", len(rows), stats)
	}
}

// "repo" in a query may be an owner: the pinned connection then reads that
// owner's collections; a value that is neither owner/repo nor an owner is
// ErrSyntax, an unknown owner ErrNotFound.
func TestQueryRepoMemberNamesAnOwner(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	ctx := context.Background()
	f.resetLog()
	cols, rows, _ := queryText(t, c, `{"collection":"issues","repo":"acme"}`)
	if len(rows) != 5 || cols[0].Source.Namespace != "acme" || !slices.Equal(searchQueries(f, "/search/issues"), []string{"org:acme is:issue"}) {
		t.Errorf("acme's issues: %d rows from %+v, searches %v", len(rows), cols[0].Source, searchQueries(f, "/search/issues"))
	}
	if len(f.requestsTo("/repos/octo/widgets/issues")) != 0 {
		t.Error("the pinned repository was listed")
	}
	f.resetLog()
	_, rows, _ = queryText(t, c, `{"collection":"repositories","repo":"hubot"}`)
	if len(rows) != 1 || len(f.requestsTo("/users/hubot/repos")) != 1 {
		t.Errorf("hubot's repositories: %d rows, %+v", len(rows), f.log())
	}
	// The owner member wins over the namespace, as a repository does.
	f.resetLog()
	_, rows, _ = queryWith(t, c, driver.Query{Text: `{"collection":"issues","repo":"hubot"}`, Namespace: "acme"})
	if len(rows) != 2 || !slices.Equal(searchQueries(f, "/search/issues"), []string{"user:hubot is:issue"}) {
		t.Errorf("repo over namespace: %d rows, searches %v", len(rows), searchQueries(f, "/search/issues"))
	}
	for _, bad := range []string{"a/b/c", "-bad", "octo/", "/widgets", "octo//widgets"} {
		_, err := c.Query(ctx, driver.Query{Text: fmt.Sprintf(`{"collection":"issues","repo":%q}`, bad)})
		wantKind(t, "repo "+bad, err, driver.ErrSyntax)
	}
	f.resetLog()
	_, err := c.Query(ctx, driver.Query{Text: `{"collection":"issues","repo":"nobody"}`})
	wantKind(t, "an unknown owner", err, driver.ErrNotFound)
	if de := asDriverError(t, err); de.Code != "404" {
		t.Errorf("code = %q, want 404", de.Code)
	}
	if len(f.requestsTo("/users/nobody")) != 1 || len(f.requestsTo("/search/issues")) != 0 {
		t.Errorf("requests = %+v", f.log())
	}
	// A collection the owner does not have is ErrNotFound naming the
	// owner's collections.
	_, err = c.Query(ctx, driver.Query{Text: `{"collection":"labels","repo":"acme"}`})
	wantKind(t, "labels of an owner", err, driver.ErrNotFound)
	if !strings.Contains(err.Error(), `collection "labels" does not exist in acme (the collections are repositories, issues, pull_requests, commits)`) {
		t.Errorf("refusal = %v", err)
	}
	_, err = c.Query(ctx, driver.Query{Text: `{"collection":"repositories","repo":"acme/alpha"}`})
	wantKind(t, "repositories of a repository", err, driver.ErrNotFound)
}

// A get in an owner scope reads through the item's repository: issues and
// pull requests by "repository" and number, repositories by full_name.
func TestGetInAnOwnerScope(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.openDSN(t, f.dsn("acme", ""), false, fakeToken)
	ctx := context.Background()
	f.resetLog()
	cols, rows, _ := queryText(t, c, `{"op":"get","collection":"issues","repository":"alpha","number":3}`)
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	if len(f.requestsTo("/repos/acme/alpha/issues/3")) != 1 || len(f.requestsTo("/search/issues")) != 0 {
		t.Errorf("requests = %+v", f.log())
	}
	if repo, _ := cell(t, cols, rows[0], "repository").AsText(); repo != "acme/alpha" {
		t.Errorf("repository = %q", repo)
	}
	if n, _ := cell(t, cols, rows[0], "number").AsInt(); n != 3 {
		t.Errorf("number = %d", n)
	}
	if title, _ := cell(t, cols, rows[0], "title").AsText(); title != "alpha issue 3" {
		t.Errorf("title = %q", title)
	}
	if cols[0].Source == nil || cols[0].Source.Namespace != "acme" || cols[0].Source.Name != "issues" || cols[len(cols)-1].Name != "repository" {
		t.Errorf("columns = %+v", cols)
	}
	// The full name and the key member work too; the repository column is
	// filled when the item lacks a repository_url.
	f.setHook(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path != "/repos/acme/beta/issues/1" {
			return false
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"number":1,"title":"beta issue 1","state":"open","labels":[],"assignees":[],"comments":0}`))
		return true
	})
	cols, rows, _ = queryText(t, c, `{"op":"get","collection":"issues","repository":"acme/beta","key":"1"}`)
	if repo, _ := cell(t, cols, rows[0], "repository").AsText(); repo != "acme/beta" {
		t.Errorf("repository = %q, want it filled from the key", repo)
	}
	f.setHook(nil)
	// Pull requests, through a user's repository, from the pinned connection
	// naming the owner.
	pinned := f.open(t, false)
	f.resetLog()
	cols, rows, _ = queryText(t, pinned, `{"op":"get","collection":"pull_requests","repo":"octo","repository":"widgets","number":252}`)
	if len(rows) != 1 || len(f.requestsTo("/repos/octo/widgets/pulls/252")) != 1 {
		t.Fatalf("%d rows, requests %+v", len(rows), f.log())
	}
	if repo, _ := cell(t, cols, rows[0], "repository").AsText(); repo != "octo/widgets" {
		t.Errorf("repository = %q", repo)
	}
	if len(cols) != len(issueColumns)+1 {
		t.Errorf("%d columns, want the issue view and the repository", len(cols))
	}
	// Repositories by full name, bare or qualified.
	f.resetLog()
	cols, rows, _ = queryText(t, c, `{"op":"get","collection":"repositories","full_name":"alpha"}`)
	if len(rows) != 1 || len(f.requestsTo("/repos/acme/alpha")) != 1 {
		t.Fatalf("repositories get: %d rows, %+v", len(rows), f.log())
	}
	if name, _ := cell(t, cols, rows[0], "full_name").AsText(); name != "acme/alpha" || !cols[0].Key {
		t.Errorf("full_name = %q", name)
	}
	if len(cols) != len(repositoryColumns) || cols[0].Source == nil || cols[0].Source.Namespace != "acme" {
		t.Errorf("columns = %v", columnNames(cols))
	}
	_, rows, _ = queryText(t, c, `{"op":"get","collection":"repositories","key":"acme/beta"}`)
	if len(rows) != 1 {
		t.Errorf("get by qualified key: %d rows", len(rows))
	}
	// Refusals, none of which sends anything.
	f.resetLog()
	_, err := c.Query(ctx, driver.Query{Text: `{"op":"get","collection":"issues","number":3}`})
	wantKind(t, "a get without the repository", err, driver.ErrSyntax)
	if !strings.Contains(err.Error(), `an owner's issues are keyed by "repository" and "number"`) {
		t.Errorf("refusal = %v", err)
	}
	_, err = c.Query(ctx, driver.Query{Text: `{"op":"get","collection":"issues","repository":"octo/widgets","number":1}`})
	wantKind(t, "a repository of another owner", err, driver.ErrSyntax)
	if !strings.Contains(err.Error(), "octo/widgets belongs to octo, not to acme") {
		t.Errorf("refusal = %v", err)
	}
	_, err = c.Query(ctx, driver.Query{Text: `{"op":"get","collection":"issues","repository":"a/b/c","number":1}`})
	wantKind(t, "a repository that is not owner/name", err, driver.ErrSyntax)
	_, err = c.Query(ctx, driver.Query{Text: `{"op":"get","collection":"repositories","full_name":"a/b/c"}`})
	wantKind(t, "a full name that is not owner/name", err, driver.ErrSyntax)
	_, err = c.Query(ctx, driver.Query{Text: `{"op":"get","collection":"commits","repository":"alpha","sha":"abc"}`})
	wantKind(t, "a commit of an owner", err, driver.ErrUnsupported)
	if !strings.Contains(err.Error(), "read by listing only") {
		t.Errorf("refusal = %v", err)
	}
	if n := len(f.log()); n != 0 {
		t.Errorf("refused gets sent %d requests: %+v", n, f.log())
	}
	_, err = c.Query(ctx, driver.Query{Text: `{"op":"get","collection":"issues","repository":"nothing","number":1}`})
	wantKind(t, "a missing repository", err, driver.ErrNotFound)
	_, err = c.Query(ctx, driver.Query{Text: `{"op":"get","collection":"issues","repository":"alpha","number":99}`})
	wantKind(t, "a missing issue", err, driver.ErrNotFound)
	_, err = c.Query(ctx, driver.Query{Text: `{"op":"get","collection":"repositories","full_name":"nothing"}`})
	wantKind(t, "a missing repository by full name", err, driver.ErrNotFound)
	// In a repository scope "repository" is not needed and is ignored.
	_, rows, _ = queryWith(t, c, driver.Query{Text: `{"op":"get","collection":"issues","repository":"alpha","number":2}`, Namespace: "acme/alpha"})
	if len(rows) != 1 {
		t.Errorf("a get with repository in a repository scope: %d rows", len(rows))
	}
}

// Mutator.

// A write on an owner's issues or pull requests routes to the repository
// named by "repository": the insert's values name it and the returned key
// carries it; update and delete keys name it. Commits and repositories are
// read-only; a missing repository is ErrSyntax.
func TestMutateOwnerIssuesRoutesToTheRepository(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.openDSN(t, f.dsn("acme", ""), false, fakeToken)
	ctx := context.Background()
	issues := driver.CollectionRef{Namespace: "acme", Name: "issues"}
	f.resetLog()
	key, err := c.Insert(ctx, issues, driver.Object{
		{Key: "repository", Value: driver.String("alpha")},
		{Key: "title", Value: driver.String("Alpha needs a handle")},
		{Key: "labels", Value: driver.List(driver.String("bug"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	writes := f.writes()
	if len(writes) != 1 || writes[0].Method != http.MethodPost || writes[0].Path != "/repos/acme/alpha/issues" {
		t.Fatalf("writes = %+v", writes)
	}
	body := sentBody(t, writes[0])
	if body["title"] != "Alpha needs a handle" || body["repository"] != nil {
		t.Errorf("body = %v, want the title without the repository", body)
	}
	if len(key) != 2 || key[0].Key != "repository" || key[1].Key != "number" {
		t.Fatalf("key = %+v, want repository and number", key)
	}
	repo, _ := key[0].Value.AsText()
	n, _ := key[1].Value.AsInt()
	if repo != "acme/alpha" || n != 5 {
		t.Errorf("key = %s #%d, want acme/alpha #5", repo, n)
	}
	if f.find("acme", "alpha", "issues", func(o object) bool { return str(o, "title") == "Alpha needs a handle" }) == nil {
		t.Error("the issue was not created in acme/alpha")
	}
	// The qualified name, into the private repository.
	f.resetLog()
	key, err = c.Insert(ctx, issues, driver.Object{{Key: "title", Value: driver.String("Beta too")}, {Key: "repository", Value: driver.String("acme/beta")}})
	if err != nil {
		t.Fatal(err)
	}
	if w := f.writes(); len(w) != 1 || w[0].Path != "/repos/acme/beta/issues" {
		t.Errorf("writes = %+v", w)
	}
	if repo, _ := key[0].Value.AsText(); repo != "acme/beta" {
		t.Errorf("key = %+v", key)
	}
	// Update and delete name the repository in the key.
	f.resetLog()
	err = c.Update(ctx, issues, driver.Object{{Key: "repository", Value: driver.String("alpha")}, {Key: "number", Value: driver.Int(n)}}, driver.Object{{Key: "state", Value: driver.String("closed")}})
	if err != nil {
		t.Fatal(err)
	}
	if w := f.writes(); len(w) != 1 || w[0].Method != http.MethodPatch || w[0].Path != fmt.Sprintf("/repos/acme/alpha/issues/%d", n) || sentBody(t, w[0])["state"] != "closed" {
		t.Errorf("writes = %+v", w)
	}
	if is := f.find("acme", "alpha", "issues", func(o object) bool { return num(o, "number") == n }); str(is, "state") != "closed" {
		t.Errorf("the issue was not closed: %v", is)
	}
	err = c.Delete(ctx, issues, driver.Object{{Key: "repository", Value: driver.String("acme/alpha")}, {Key: "number", Value: driver.Int(n)}})
	wantKind(t, "deleting an issue", err, driver.ErrUnsupported)
	if !strings.Contains(err.Error(), "close it instead") {
		t.Errorf("refusal = %v", err)
	}
	// Pull requests, through the pinned connection naming a user.
	pinned := f.open(t, false)
	f.resetLog()
	key, err = pinned.Insert(ctx, driver.CollectionRef{Namespace: "octo", Name: "pull_requests"}, driver.Object{
		{Key: "repository", Value: driver.String("widgets")}, {Key: "title", Value: driver.String("Handle")},
		{Key: "head", Value: driver.String("feature/handle")}, {Key: "base", Value: driver.String("main")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if w := f.writes(); len(w) != 1 || w[0].Path != "/repos/octo/widgets/pulls" {
		t.Errorf("writes = %+v", w)
	}
	if repo, _ := key[0].Value.AsText(); repo != "octo/widgets" || key[1].Key != "number" {
		t.Errorf("key = %+v", key)
	}
	err = pinned.Update(ctx, driver.CollectionRef{Namespace: "octo", Name: "pull_requests"}, driver.Object{{Key: "number", Value: key[1].Value}, {Key: "repository", Value: driver.String("octo/widgets")}}, driver.Object{{Key: "title", Value: driver.String("A handle")}})
	if err != nil {
		t.Errorf("updating the pull request: %v", err)
	}
	// Refusals: none of them writes.
	f.resetLog()
	_, err = c.Insert(ctx, issues, driver.Object{{Key: "title", Value: driver.String("Nowhere")}})
	wantKind(t, "an insert without the repository", err, driver.ErrSyntax)
	if !strings.Contains(err.Error(), `keyed by "repository" and "number": name the repository`) {
		t.Errorf("refusal = %v", err)
	}
	err = c.Update(ctx, issues, driver.Object{{Key: "number", Value: driver.Int(1)}}, driver.Object{{Key: "title", Value: driver.String("x")}})
	wantKind(t, "an update without the repository", err, driver.ErrSyntax)
	err = c.Delete(ctx, issues, driver.Object{{Key: "number", Value: driver.Int(1)}})
	wantKind(t, "a delete without the repository", err, driver.ErrSyntax)
	_, err = c.Insert(ctx, issues, driver.Object{{Key: "repository", Value: driver.String("octo/widgets")}, {Key: "title", Value: driver.String("x")}})
	wantKind(t, "another owner's repository", err, driver.ErrSyntax)
	if !strings.Contains(err.Error(), "belongs to octo, not to acme") {
		t.Errorf("refusal = %v", err)
	}
	_, err = c.Insert(ctx, issues, driver.Object{{Key: "repository", Value: driver.String("a/b/c")}, {Key: "title", Value: driver.String("x")}})
	wantKind(t, "a repository that is not owner/name", err, driver.ErrSyntax)
	_, err = c.Insert(ctx, issues, driver.Object{{Key: "repository", Value: driver.Int(7)}, {Key: "title", Value: driver.String("x")}})
	wantKind(t, "a repository that is a number", err, driver.ErrNotFound)
	for _, name := range []string{"commits", "repositories"} {
		ref := driver.CollectionRef{Namespace: "acme", Name: name}
		_, err = c.Insert(ctx, ref, driver.Object{{Key: "repository", Value: driver.String("alpha")}, {Key: "message", Value: driver.String("x")}})
		wantKind(t, "Insert into "+name, err, driver.ErrUnsupported)
		if !strings.Contains(err.Error(), "read-only") {
			t.Errorf("%s: refusal = %v", name, err)
		}
		err = c.Update(ctx, ref, driver.Object{{Key: "full_name", Value: driver.String("acme/alpha")}}, driver.Object{{Key: "description", Value: driver.String("x")}})
		wantKind(t, "Update of "+name, err, driver.ErrUnsupported)
		err = c.Delete(ctx, ref, driver.Object{{Key: "sha", Value: driver.String("abc")}})
		wantKind(t, "Delete of "+name, err, driver.ErrUnsupported)
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("refused writes reached the server: %+v", w)
	}
	// A repository that does not exist is ErrNotFound before the write.
	_, err = c.Insert(ctx, issues, driver.Object{{Key: "repository", Value: driver.String("nothing")}, {Key: "title", Value: driver.String("x")}})
	wantKind(t, "a missing repository", err, driver.ErrNotFound)
	if w := f.writes(); len(w) != 0 {
		t.Errorf("a write to a missing repository was sent: %+v", w)
	}
	// Writes drop the names kept for completion, as in a repository.
	completeAt(t, c, `{"repo":"`)
	if _, err := c.Insert(ctx, issues, driver.Object{{Key: "repository", Value: driver.String("alpha")}, {Key: "title", Value: driver.String("Again")}}); err != nil {
		t.Fatal(err)
	}
	c.catalog.mu.Lock()
	cached := c.catalog.names != nil
	c.catalog.mu.Unlock()
	if cached {
		t.Error("an owner-scope insert kept the completion names")
	}
}

// On a read-only connection every write in an owner scope is ErrReadOnly
// before anything else, a missing repository or a read-only collection
// included, and nothing is sent.
func TestReadOnlyRefusesOwnerWrites(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.openDSN(t, f.dsn("acme", ""), true, fakeToken)
	ctx := context.Background()
	f.resetLog()
	for _, col := range ownerCollections {
		ref := driver.CollectionRef{Namespace: "acme", Name: col.name}
		_, err := c.Insert(ctx, ref, driver.Object{{Key: "repository", Value: driver.String("alpha")}, {Key: "title", Value: driver.String("x")}})
		wantKind(t, "Insert into "+col.name, err, driver.ErrReadOnly)
		_, err = c.Insert(ctx, ref, driver.Object{{Key: "title", Value: driver.String("x")}})
		wantKind(t, "Insert without a repository into "+col.name, err, driver.ErrReadOnly)
		key := driver.Object{{Key: "repository", Value: driver.String("alpha")}, {Key: col.keyCol, Value: driver.String("1")}}
		err = c.Update(ctx, ref, key, driver.Object{{Key: "title", Value: driver.String("x")}})
		wantKind(t, "Update of "+col.name, err, driver.ErrReadOnly)
		err = c.Delete(ctx, ref, key)
		wantKind(t, "Delete of "+col.name, err, driver.ErrReadOnly)
		if !strings.Contains(err.Error(), "read-only") {
			t.Errorf("%s: %v", col.name, err)
		}
	}
	// The same through an empty namespace, which is the owner.
	_, err := c.Insert(ctx, driver.CollectionRef{Name: "issues"}, driver.Object{{Key: "repository", Value: driver.String("alpha")}, {Key: "title", Value: driver.String("x")}})
	wantKind(t, "Insert without a namespace", err, driver.ErrReadOnly)
	if n := len(f.log()); n != 0 {
		t.Errorf("read-only writes sent %d requests: %+v", n, f.log())
	}
	if f.count("acme", "alpha", "issues") != 4 {
		t.Error("a read-only write changed the data")
	}
	// Reads still run.
	if _, rows, _ := queryText(t, c, `{"collection":"issues"}`); len(rows) != 5 {
		t.Errorf("a read-only list: %d rows", len(rows))
	}
	if _, rows, _ := browse(t, c, driver.CollectionRef{Name: "repositories"}, driver.BrowseOptions{}); len(rows) != 2 {
		t.Errorf("a read-only browse: %d rows", len(rows))
	}
}
