package github

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// explain runs Explain and fails the test on an error.
func explain(t *testing.T, c *conn, q driver.Query) *driver.Plan {
	t.Helper()
	plan, err := c.Explain(context.Background(), q, driver.ExplainOptions{})
	if err != nil {
		t.Fatalf("explain %s: %v", q.Text, err)
	}
	return plan
}

// Explain lists the requests each op would send, one child per request,
// without sending any of them.
func TestExplainPlansTheRequestsOfEachOp(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	base := f.base()
	f.resetLog()
	search := url.Values{"q": {"repo:octo/widgets widget is:pr"}, "per_page": {"50"}, "page": {"1"}, "sort": {"updated"}}
	tests := []struct {
		name  string
		text  string
		label string
		lines []string
		note  string
	}{
		{"list of 250 is three pages", `{"op":"list","collection":"issues","limit":250}`, "list", []string{
			"GET " + base + "/repos/octo/widgets/issues?page=1&per_page=100&state=all",
			"GET " + base + "/repos/octo/widgets/issues?page=2&per_page=100&state=all",
			"GET " + base + "/repos/octo/widgets/issues?page=3&per_page=100&state=all",
		}, "up to 3 requests of 100 items"},
		{"list with the endpoint's parameters", `{"collection":"pull_requests","state":"open","base":"main","limit":20}`, "list", []string{
			"GET " + base + "/repos/octo/widgets/pulls?base=main&page=1&per_page=20&state=open",
		}, "up to 1 requests of 20 items"},
		{"list of another repository", `{"collection":"labels","repo":"acme/alpha"}`, "list", []string{
			"GET " + base + "/repos/acme/alpha/labels?page=1&per_page=100",
		}, "each request counts against the core rate limit"},
		{"get", `{"op":"get","collection":"issues","number":5}`, "get", []string{
			"GET " + base + "/repos/octo/widgets/issues/5",
		}, "one request"},
		{"get of a tag scans the listing", `{"op":"get","collection":"tags","name":"v1.0.0"}`, "get", []string{
			"GET " + base + "/repos/octo/widgets/tags?page=1&per_page=100",
		}, "tags have no single-item endpoint: the listing is read page by page (up to 10 requests)"},
		{"search of pull requests adds is:pr", `{"op":"search","type":"pull_requests","q":"repo:octo/widgets widget","limit":50,"sort":"updated"}`, "search", []string{
			"GET " + base + "/search/issues?" + search.Encode(),
		}, "up to 1 requests against the search rate limit"},
		{"search of 300 repositories", `{"op":"search","type":"repositories","q":"widgets","limit":300}`, "search", []string{
			"GET " + base + "/search/repositories?page=1&per_page=100&q=widgets",
			"GET " + base + "/search/repositories?page=2&per_page=100&q=widgets",
			"GET " + base + "/search/repositories?page=3&per_page=100&q=widgets",
		}, "GitHub returns at most 1000 results"},
		{"repositories of a pinned DSN", `{"op":"repositories"}`, "repositories", []string{
			"GET " + base + "/repos/octo/widgets",
		}, "one request"},
		{"rest GET pages", `{"op":"rest","path":"/repos/octo/widgets/labels","params":{"sort":"name"}}`, "rest", []string{
			"GET " + base + "/repos/octo/widgets/labels?per_page=100&sort=name",
		}, "one request, then up to 0 more when the answer is a paginated array"},
		{"rest GET with a limit", `{"op":"rest","path":"/repos/octo/widgets/labels","limit":250}`, "rest", []string{
			"GET " + base + "/repos/octo/widgets/labels?per_page=100",
		}, "then up to 2 more"},
		{"rest POST", `{"op":"rest","method":"post","path":"/repos/octo/widgets/labels","body":{"name":"x"}}`, "rest", []string{
			"POST " + base + "/repos/octo/widgets/labels",
		}, "one request that writes"},
		{"graphql", `{"op":"graphql","query":"{ viewer { login } }"}`, "graphql", []string{
			"POST " + base + "/graphql",
		}, "GraphQL rate limit"},
		{"rate_limit", `{"op":"rate_limit"}`, "rate_limit", []string{
			"GET " + base + "/rate_limit",
		}, "one request that costs nothing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := explain(t, c, driver.Query{Text: tc.text})
			if plan.Format != "text" || plan.Root == nil {
				t.Fatalf("plan = %+v", plan)
			}
			lines := strings.Split(plan.Text, "\n")
			if len(lines) != len(tc.lines)+1 {
				t.Fatalf("text = %q, want %d request lines and the note", plan.Text, len(tc.lines))
			}
			for i, want := range tc.lines {
				if lines[i] != want {
					t.Errorf("line %d = %q, want %q", i, lines[i], want)
				}
			}
			if last := lines[len(lines)-1]; last != plan.Root.Detail || !strings.Contains(last, tc.note) {
				t.Errorf("note = %q, want the root's detail mentioning %q", last, tc.note)
			}
			root := plan.Root
			if root.Label != tc.label || root.Cost != float64(len(tc.lines)) || root.Rows != -1 || len(root.Children) != len(tc.lines) {
				t.Errorf("root = %+v", root)
			}
			for i, child := range root.Children {
				method, u, _ := strings.Cut(tc.lines[i], " ")
				if child.Label != method || child.Detail != u || child.Cost != 1 || child.Rows != -1 || len(child.Children) != 0 {
					t.Errorf("child %d = %+v", i, child)
				}
			}
		})
	}
	if n := len(f.log()); n != 0 {
		t.Errorf("Explain sent %d requests: %+v", n, f.log())
	}
}

// Without a pinned repository the repositories op is planned as the
// owner's listing (with the note that an organization's lives elsewhere),
// or the token's own.
func TestExplainRepositoriesWithoutAPinnedRepository(t *testing.T) {
	f := newFakeGitHub(t)
	base := f.base()
	owner := f.openDSN(t, f.dsn("octo", ""), false, fakeToken)
	f.resetLog()
	plan := explain(t, owner, driver.Query{Text: `{"op":"repositories","limit":30}`})
	if lines := strings.Split(plan.Text, "\n"); lines[0] != "GET "+base+"/users/octo/repos?per_page=30&page=1" || !strings.Contains(plan.Root.Detail, "/orgs/octo/repos") {
		t.Errorf("owner plan = %q", plan.Text)
	}
	mine := f.openDSN(t, f.dsn("", ""), false, fakeToken)
	f.resetLog()
	plan = explain(t, mine, driver.Query{Text: `{"op":"repositories"}`})
	if lines := strings.Split(plan.Text, "\n"); lines[0] != "GET "+base+"/user/repos?per_page=100&page=1" || plan.Root.Cost != 1 {
		t.Errorf("token plan = %q", plan.Text)
	}
	// A list query on such a connection takes its repository from the
	// query or the namespace.
	plan = explain(t, owner, driver.Query{Text: `{"collection":"labels"}`, Namespace: "octo/gadgets"})
	if !strings.HasPrefix(plan.Text, "GET "+base+"/repos/octo/gadgets/labels?") {
		t.Errorf("namespace plan = %q", plan.Text)
	}
	plan = explain(t, owner, driver.Query{Text: `{"collection":"labels","repo":"hubot/scripts"}`, Namespace: "octo/gadgets"})
	if !strings.HasPrefix(plan.Text, "GET "+base+"/repos/hubot/scripts/labels?") {
		t.Errorf("the query's repo does not win over the namespace: %q", plan.Text)
	}
	if n := len(f.log()); n != 0 {
		t.Errorf("Explain sent %d requests: %+v", n, f.log())
	}
}

// qualifiersOf reads the terms of the q of a planned search URL.
func qualifiersOf(t *testing.T, line string) []string {
	t.Helper()
	_, rawURL, _ := strings.Cut(line, " ")
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", line, err)
	}
	return strings.Fields(u.Query().Get("q"))
}

// In an owner scope a list is planned as the search requests with the
// owner's qualifier (org: or user: once the owner has been looked up on
// the connection, both spellings before), repositories as the user listing
// with a note on the three listings, and a get as the repository's own
// request; nothing is sent.
func TestExplainInAnOwnerScope(t *testing.T) {
	f := newFakeGitHub(t)
	base := f.base()
	ctx := context.Background()
	fresh := f.openDSN(t, f.dsn("acme", ""), false, fakeToken)
	pinned := f.open(t, false)
	f.resetLog()
	plan := explain(t, fresh, driver.Query{Text: `{"collection":"pull_requests"}`})
	lines := strings.Split(plan.Text, "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "GET "+base+"/search/issues?") || !strings.Contains(lines[0], url.QueryEscape("org:acme (or user:acme) is:pr")) {
		t.Errorf("plan before the owner is known = %q", plan.Text)
	}
	if !strings.Contains(plan.Root.Detail, "search rate limit") || !strings.Contains(plan.Root.Detail, "at most 1000 results") {
		t.Errorf("note = %q", plan.Root.Detail)
	}
	// Once the owner has been looked up on this connection, its kind is known.
	if _, err := fresh.Collections(ctx, driver.NamespaceRef{Name: "acme"}); err != nil {
		t.Fatal(err)
	}
	f.resetLog()
	tests := []struct {
		name  string
		text  string
		lines []string
		q     []string
		note  string
	}{
		{"issues of 150 are two searches", `{"collection":"issues","limit":150}`, []string{
			"GET " + base + "/search/issues?page=1&per_page=100&q=org%3Aacme+is%3Aissue",
			"GET " + base + "/search/issues?page=2&per_page=100&q=org%3Aacme+is%3Aissue",
		}, nil, "up to 2 requests of 100 items"},
		{"q, sort and order", `{"collection":"issues","q":"label:bug","sort":"created","order":"desc","limit":10}`, nil,
			[]string{"org:acme", "is:issue", "label:bug"}, "search rate limit"},
		{"pull requests", `{"collection":"pull_requests","limit":5}`, []string{
			"GET " + base + "/search/issues?page=1&per_page=5&q=org%3Aacme+is%3Apr",
		}, nil, "GitHub returns at most 1000 results"},
		{"commits", `{"collection":"commits","q":"typo"}`, nil, []string{"org:acme", "typo"}, "search rate limit"},
		{"repositories", `{"collection":"repositories","limit":30}`, []string{
			"GET " + base + "/users/acme/repos?page=1&per_page=30",
		}, nil, "the listing is /users/acme/repos for a user, /orgs/acme/repos for an organization, /user/repos for the token's own login"},
		{"repositories with the listing's parameters", `{"collection":"repositories","type":"all","sort":"created","direction":"asc"}`, []string{
			"GET " + base + "/users/acme/repos?direction=asc&page=1&per_page=100&sort=created&type=all",
		}, nil, "core rate limit"},
		{"get through the repository", `{"op":"get","collection":"issues","repository":"alpha","number":3}`, []string{
			"GET " + base + "/repos/acme/alpha/issues/3",
		}, nil, "one request"},
		{"get with the qualified repository", `{"op":"get","collection":"pull_requests","repository":"acme/beta","number":1}`, []string{
			"GET " + base + "/repos/acme/beta/pulls/1",
		}, nil, "one request"},
		{"get of a repository by full name", `{"op":"get","collection":"repositories","full_name":"beta"}`, []string{
			"GET " + base + "/repos/acme/beta",
		}, nil, "one request"},
		{"get of a repository by qualified name", `{"op":"get","collection":"repositories","full_name":"acme/alpha"}`, []string{
			"GET " + base + "/repos/acme/alpha",
		}, nil, "one request"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := explain(t, fresh, driver.Query{Text: tc.text})
			lines := strings.Split(plan.Text, "\n")
			if tc.lines != nil {
				if len(lines) != len(tc.lines)+1 {
					t.Fatalf("text = %q, want %d request lines and the note", plan.Text, len(tc.lines))
				}
				for i, want := range tc.lines {
					if lines[i] != want {
						t.Errorf("line %d = %q, want %q", i, lines[i], want)
					}
				}
			}
			if tc.q != nil {
				got := qualifiersOf(t, lines[0])
				slices.Sort(got)
				want := slices.Clone(tc.q)
				slices.Sort(want)
				if !slices.Equal(got, want) || !strings.HasPrefix(lines[0], "GET "+base+"/search/") {
					t.Errorf("line %q has qualifiers %v, want %v", lines[0], got, tc.q)
				}
			}
			if !strings.Contains(plan.Root.Detail, tc.note) || plan.Root.Cost != float64(len(plan.Root.Children)) || plan.Root.Label == "" {
				t.Errorf("root = %+v, want the note mentioning %q", plan.Root, tc.note)
			}
		})
	}
	// Sort and order travel as parameters.
	plan = explain(t, fresh, driver.Query{Text: `{"collection":"issues","sort":"updated","order":"asc"}`})
	if !strings.Contains(plan.Text, "order=asc&") || !strings.Contains(plan.Text, "&sort=updated") {
		t.Errorf("sorted plan = %q", plan.Text)
	}
	// The owner may come from the query or the namespace, on any connection.
	plan = explain(t, pinned, driver.Query{Text: `{"collection":"issues","repo":"acme"}`})
	if !strings.HasPrefix(plan.Text, "GET "+base+"/search/issues?") {
		t.Errorf("owner from repo = %q", plan.Text)
	}
	plan = explain(t, pinned, driver.Query{Text: `{"collection":"repositories"}`, Namespace: "hubot"})
	if !strings.HasPrefix(plan.Text, "GET "+base+"/users/hubot/repos?") {
		t.Errorf("owner from the namespace = %q", plan.Text)
	}
	// Refusals are the query's.
	_, err := fresh.Explain(ctx, driver.Query{Text: `{"collection":"issues","state":"open"}`}, driver.ExplainOptions{})
	wantKind(t, "a repository parameter", err, driver.ErrSyntax)
	_, err = fresh.Explain(ctx, driver.Query{Text: `{"op":"get","collection":"issues","number":3}`}, driver.ExplainOptions{})
	wantKind(t, "a get without the repository", err, driver.ErrSyntax)
	_, err = fresh.Explain(ctx, driver.Query{Text: `{"op":"get","collection":"issues","repository":"octo/widgets","number":3}`}, driver.ExplainOptions{})
	wantKind(t, "another owner's repository", err, driver.ErrSyntax)
	_, err = fresh.Explain(ctx, driver.Query{Text: `{"op":"get","collection":"commits","repository":"alpha","sha":"abc"}`}, driver.ExplainOptions{})
	wantKind(t, "a commit of an owner", err, driver.ErrUnsupported)
	_, err = fresh.Explain(ctx, driver.Query{Text: `{"collection":"labels"}`}, driver.ExplainOptions{})
	wantKind(t, "a repository collection of an owner", err, driver.ErrNotFound)
	if n := len(f.log()); n != 0 {
		t.Errorf("Explain sent %d requests: %+v", n, f.log())
	}
}

// Explain refuses what the query would refuse, plus Analyze; nothing is
// sent in either case.
func TestExplainRefusals(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	ctx := context.Background()
	f.resetLog()
	_, err := c.Explain(ctx, driver.Query{Text: `{"collection":"labels"}`}, driver.ExplainOptions{Analyze: true})
	wantKind(t, "analyze", err, driver.ErrUnsupported)
	if !strings.Contains(err.Error(), "GitHub has no analyze") {
		t.Errorf("analyze refusal = %v", err)
	}
	_, err = c.Explain(ctx, driver.Query{Text: `{"collection":"labels"}`, Params: []driver.Value{driver.Int(1)}}, driver.ExplainOptions{})
	wantKind(t, "positional parameters", err, driver.ErrSyntax)
	_, err = c.Explain(ctx, driver.Query{Text: `{"op":"rest","method":"POST","path":"/repos/octo/widgets/labels","body":{"name":"x"}}`, ReadOnly: true}, driver.ExplainOptions{})
	wantKind(t, "a rest POST under Query.ReadOnly", err, driver.ErrReadOnly)
	_, err = c.Explain(ctx, driver.Query{Text: `{"op":"graphql","query":"mutation { x }"}`, ReadOnly: true}, driver.ExplainOptions{})
	wantKind(t, "a mutation under Query.ReadOnly", err, driver.ErrReadOnly)
	_, err = c.Explain(ctx, driver.Query{Text: `{"collection":"wiki"}`}, driver.ExplainOptions{})
	wantKind(t, "a missing collection", err, driver.ErrNotFound)
	_, err = c.Explain(ctx, driver.Query{Text: `{"op":"fly"}`}, driver.ExplainOptions{})
	wantKind(t, "an unknown op", err, driver.ErrSyntax)
	_, err = c.Explain(ctx, driver.Query{Text: `{"op":"get","collection":"issues"}`}, driver.ExplainOptions{})
	wantKind(t, "a get without a key", err, driver.ErrSyntax)
	_, err = c.Explain(ctx, driver.Query{Text: `not json`}, driver.ExplainOptions{})
	wantKind(t, "malformed text", err, driver.ErrSyntax)

	// Without a repository or an owner anywhere (a bare DSN and no token to
	// name a login) there is no scope to plan in.
	nobody := f.openDSN(t, f.dsn("", ""), false, "")
	owner := f.openDSN(t, f.dsn("octo", ""), false, fakeToken)
	f.resetLog()
	_, err = nobody.Explain(ctx, driver.Query{Text: `{"collection":"issues"}`}, driver.ExplainOptions{})
	wantKind(t, "no repository or owner anywhere", err, driver.ErrSyntax)
	if !strings.Contains(err.Error(), "name the repository or owner") {
		t.Errorf("missing scope refusal = %v", err)
	}
	_, err = owner.Explain(ctx, driver.Query{Text: `{"op":"get","collection":"issues","number":1}`, Namespace: "not/a/repo"}, driver.ExplainOptions{})
	wantKind(t, "a namespace that is not owner/repo", err, driver.ErrNotFound)
	_, err = owner.Explain(ctx, driver.Query{Text: `{"collection":"issues","state":"open"}`}, driver.ExplainOptions{})
	wantKind(t, "a repository parameter in an owner scope", err, driver.ErrSyntax)
	if !strings.Contains(err.Error(), `"state" is not a parameter of issues in octo`) {
		t.Errorf("owner parameter refusal = %v", err)
	}
	if n := len(f.log()); n != 0 {
		t.Errorf("refused explains sent %d requests: %+v", n, f.log())
	}

	closed := f.open(t, false)
	_ = closed.Close()
	_, err = closed.Explain(ctx, driver.Query{Text: `{"collection":"labels"}`}, driver.ExplainOptions{})
	wantKind(t, "a closed connection", err, driver.ErrConnection)
}
