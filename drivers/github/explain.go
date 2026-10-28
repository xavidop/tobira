package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// Explain shows the HTTP requests a query would make and what they cost
// against the rate limit. It never sends anything, so Analyze is
// unsupported, and a write refused read-only is refused here too.
func (c *conn) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	const op = "explain"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if opts.Analyze {
		return nil, unsupportedErr(op, "GitHub has no analyze: explain shows the requests without sending them")
	}
	if len(q.Params) > 0 {
		return nil, syntaxErr(op, "github queries take no positional parameters")
	}
	r, err := parseRequest(q.Text, c.readOnly || q.ReadOnly)
	if err != nil {
		return nil, err
	}
	reqs, note, err := c.plannedRequests(ctx, op, r, q)
	if err != nil {
		return nil, err
	}
	root := &driver.PlanNode{Label: r.op, Detail: note, Cost: float64(len(reqs)), Rows: -1}
	var lines []string
	for _, pr := range reqs {
		root.Children = append(root.Children, &driver.PlanNode{Label: pr.method, Detail: pr.url, Cost: 1, Rows: -1})
		lines = append(lines, pr.method+" "+pr.url)
	}
	if note != "" {
		lines = append(lines, note)
	}
	return &driver.Plan{Format: "text", Text: strings.Join(lines, "\n"), Root: root}, nil
}

// plannedRequest is one request a query would send.
type plannedRequest struct{ method, url string }

// cachedOwnerQualifier is the owner's search qualifier when the owner has
// been looked up, else both spellings.
func (c *conn) cachedOwnerQualifier(owner string) string {
	c.repos.mu.Lock()
	info, ok := c.repos.owners[strings.ToLower(owner)]
	c.repos.mu.Unlock()
	switch {
	case ok && info.org:
		return "org:" + info.login
	case ok:
		return "user:" + info.login
	}
	return "org:" + owner + " (or user:" + owner + ")"
}

// plannedGet is the request of a get query.
func (c *conn) plannedGet(op string, col *collection, s scope, r request, costNote string) ([]plannedRequest, string, error) {
	if s.ownerWide() {
		if col.listing {
			full := r.key
			if !strings.Contains(full, "/") {
				full = s.owner + "/" + full
			}
			return []plannedRequest{{http.MethodGet, c.client.url("/repos/"+full, nil)}}, "one request; " + costNote, nil
		}
		peer, rs, err := c.ownerItemScope(op, col, s, r.repository)
		if err != nil {
			return nil, "", err
		}
		col, s = peer, rs
	}
	if col.get == nil {
		req := listRequest{col: col, owner: s.owner, repo: s.repo, query: url.Values{}, perPage: maxPerPage}
		return []plannedRequest{{http.MethodGet, c.client.url(req.path(), req.pageQuery(1))}},
			fmt.Sprintf("%s have no single-item endpoint: the listing is read page by page (up to %d requests) until the key is found; %s", col.name, maxListRows/maxPerPage, costNote), nil
	}
	return []plannedRequest{{http.MethodGet, c.client.url(col.get(s.owner, s.repo, r.key), nil)}}, "one request; " + costNote, nil
}

// plannedRequests lists the requests of a parsed query, with a note on the
// rate-limit cost. Nothing is sent: a repository or owner is not looked up,
// and an owner's kind (org: or user:) comes from the cache when it has been
// seen, else both spellings are shown.
func (c *conn) plannedRequests(ctx context.Context, op string, r request, q driver.Query) ([]plannedRequest, string, error) {
	const costNote = "each request counts against the core rate limit (5,000 per hour with a token, 60 without); a page unchanged since the last read answers 304 and costs nothing"
	const searchNote = "each request counts against the search rate limit (30 per minute with a token, 10 without); GitHub returns at most 1000 results"
	pages := func(limit int) int { return (limit + maxPerPage - 1) / maxPerPage }
	switch r.op {
	case opList, opGet:
		s, err := c.resolveScope(ctx, op, firstNonEmpty(r.repo, q.Namespace))
		if err != nil {
			return nil, "", err
		}
		col := collectionIn(s, r.collection)
		if col == nil {
			return nil, "", notFoundErr(op, "collection %q does not exist in %s (the collections are %s)", r.collection, s.name(), strings.Join(scopeCollectionNames(s), ", "))
		}
		for k := range r.params {
			if !slices.Contains(col.params, k) {
				return nil, "", syntaxErr(op, "%q is not a parameter of %s in %s (%s)", k, col.name, s.name(), strings.Join(col.params, ", "))
			}
		}
		if r.op == opGet {
			return c.plannedGet(op, col, s, r, costNote)
		}
		limit, _ := rowCap(r, q)
		req := listRequest{col: col, owner: s.owner, repo: s.repo, query: url.Values{}, perPage: min(limit, maxPerPage)}
		for k, v := range col.defaults {
			req.query[k] = v
		}
		for k, v := range r.params {
			req.query[k] = v
		}
		note := costNote
		switch {
		case col.search != nil:
			q, err := searchQuery(op, col, c.cachedOwnerQualifier(s.owner), r.params.Get("q"))
			if err != nil {
				return nil, "", err
			}
			req.query.Set("q", q)
			note = searchNote
		case col.listing:
			req.listPath = "/users/" + url.PathEscape(s.owner) + "/repos"
			note = "the listing is /users/" + s.owner + "/repos for a user, /orgs/" + s.owner + "/repos for an organization, /user/repos for the token's own login; " + costNote
		}
		var out []plannedRequest
		for page := 1; page <= pages(limit); page++ {
			out = append(out, plannedRequest{http.MethodGet, c.client.url(req.path(), req.pageQuery(page))})
		}
		return out, fmt.Sprintf("up to %d requests of %d items, fewer when the listing ends sooner; %s", pages(limit), req.perPage, note), nil
	case opSearch:
		limit, _ := rowCap(r, q)
		endpoint, text := "/search/"+r.searchType, r.q
		if r.searchType == "pull_requests" {
			endpoint, text = "/search/issues", r.q+" is:pr"
		}
		var out []plannedRequest
		for page := 1; page <= pages(limit); page++ {
			query := url.Values{"q": {text}, "per_page": {strconv.Itoa(min(limit, maxPerPage))}, "page": {strconv.Itoa(page)}}
			for k, v := range r.params {
				query[k] = v
			}
			out = append(out, plannedRequest{http.MethodGet, c.client.url(endpoint, query)})
		}
		return out, fmt.Sprintf("up to %d requests against the search rate limit (30 per minute with a token, 10 without); GitHub returns at most %d results", pages(limit), maxListRows), nil
	case opRepositories:
		if c.dsn.repo != "" {
			return []plannedRequest{{http.MethodGet, c.client.url("/repos/"+url.PathEscape(c.dsn.owner)+"/"+url.PathEscape(c.dsn.repo), nil)}}, "one request; " + costNote, nil
		}
		limit, _ := rowCap(r, q)
		path, note := "/user/repos", fmt.Sprintf("up to %d requests; %s", pages(limit), costNote)
		if c.dsn.owner != "" {
			path = "/users/" + url.PathEscape(c.dsn.owner) + "/repos"
			note = fmt.Sprintf("up to %d requests, after one that tells a user from an organization (whose repositories are under /orgs/%s/repos); %s", pages(limit), url.PathEscape(c.dsn.owner), costNote)
		}
		return []plannedRequest{{http.MethodGet, c.client.base + path + "?per_page=" + strconv.Itoa(min(limit, maxPerPage)) + "&page=1"}}, note, nil
	case opRest:
		query := url.Values{}
		for k, v := range r.params {
			query[k] = v
		}
		note := "one request; " + costNote
		if r.method == http.MethodGet && query.Get("page") == "" {
			limit, _ := rowCap(r, q)
			if query.Get("per_page") == "" {
				query.Set("per_page", strconv.Itoa(min(limit, maxPerPage)))
			}
			note = fmt.Sprintf("one request, then up to %d more when the answer is a paginated array; %s", pages(limit)-1, costNote)
		}
		if r.method != http.MethodGet {
			note = "one request that writes; " + costNote
		}
		return []plannedRequest{{r.method, c.client.url(r.path, query)}}, note, nil
	case opGraphQL:
		return []plannedRequest{{http.MethodPost, c.client.graphql}}, "one request against the GraphQL rate limit (5,000 points per hour; the server bills a query by the nodes it touches)", nil
	case opRateLimit:
		return []plannedRequest{{http.MethodGet, c.client.url("/rate_limit", nil)}}, "one request that costs nothing", nil
	}
	return nil, "", syntaxErr(op, "unknown op %q", r.op)
}
