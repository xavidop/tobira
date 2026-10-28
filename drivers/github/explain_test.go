package github

import (
	"context"
	"net/url"
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

	owner := f.openDSN(t, f.dsn("octo", ""), false, fakeToken)
	f.resetLog()
	_, err = owner.Explain(ctx, driver.Query{Text: `{"collection":"issues"}`}, driver.ExplainOptions{})
	wantKind(t, "no repository anywhere", err, driver.ErrSyntax)
	if !strings.Contains(err.Error(), "name the repository") {
		t.Errorf("missing repository refusal = %v", err)
	}
	_, err = owner.Explain(ctx, driver.Query{Text: `{"op":"get","collection":"issues","number":1}`, Namespace: "not-a-repo"}, driver.ExplainOptions{})
	wantKind(t, "a namespace that is not owner/repo", err, driver.ErrNotFound)
	if n := len(f.log()); n != 0 {
		t.Errorf("refused explains sent %d requests: %+v", n, f.log())
	}

	closed := f.open(t, false)
	_ = closed.Close()
	_, err = closed.Explain(ctx, driver.Query{Text: `{"collection":"labels"}`}, driver.ExplainOptions{})
	wantKind(t, "a closed connection", err, driver.ErrConnection)
}
