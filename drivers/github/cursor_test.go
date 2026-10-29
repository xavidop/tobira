package github

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestCursor(t *testing.T) {
	t.Parallel()
	cur := browseCursor{page: 3, skip: 7, hash: "abc123"}
	encoded := cur.encode()
	if encoded != "p|3|7|abc123" {
		t.Errorf("encode = %q", encoded)
	}
	got, err := decodeCursor(encoded, "abc123")
	if err != nil || got != cur {
		t.Errorf("decode(encode) = %+v, %v; want %+v", got, err, cur)
	}
	got, err = decodeCursor("", "abc123")
	if err != nil || got != (browseCursor{page: 1, hash: "abc123"}) {
		t.Errorf("decode(\"\") = %+v, %v; want page 1", got, err)
	}
	for s, hint := range map[string]string{
		"p|3|7|other":      "other filters, sorting or page size",
		"x":                "not a github browse cursor",
		"p|1|0":            "not a github browse cursor",
		"p|1|0|abc123|x":   "not a github browse cursor",
		"q|1|0|abc123":     "not a github browse cursor",
		"p|a|0|abc123":     "bad page or offset",
		"p|1|b|abc123":     "bad page or offset",
		"p|0|0|abc123":     "bad page or offset",
		"p|-1|0|abc123":    "bad page or offset",
		"p|1|-1|abc123":    "bad page or offset",
		"p|1.5|0|abc123":   "bad page or offset",
		"p| 1|0|abc123":    "bad page or offset",
		"p|1|0|":           "other filters, sorting or page size",
		"eyJwYWdlIjozfQ==": "not a github browse cursor",
	} {
		_, err := decodeCursor(s, "abc123")
		if !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("decode(%q) = %v, want ErrSyntax", s, err)
			continue
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Op != "browse" || de.Driver != Name {
			t.Errorf("decode(%q) attribution = %+v", s, de)
		}
		if !strings.Contains(err.Error(), "invalid cursor: ") || !strings.Contains(err.Error(), hint) {
			t.Errorf("decode(%q) = %v, want %q", s, err, hint)
		}
	}
}

func TestListRequest(t *testing.T) {
	t.Parallel()
	issues := collectionByName("issues")
	base := listRequest{col: issues, owner: "octo", repo: "widgets", query: url.Values{"state": {"open"}, "labels": {"bug"}}, perPage: 50}
	if base.path() != "/repos/octo/widgets/issues" {
		t.Errorf("path = %q", base.path())
	}
	t.Run("hash", func(t *testing.T) {
		t.Parallel()
		same := listRequest{col: issues, owner: "Octo", repo: "WIDGETS", query: url.Values{"labels": {"bug"}, "state": {"open"}}, perPage: 50}
		if base.hash() != same.hash() {
			t.Errorf("hash is not stable or depends on repository case or map order: %q vs %q", base.hash(), same.hash())
		}
		if len(base.hash()) == 0 || strings.ContainsAny(base.hash(), "|") {
			t.Errorf("hash %q must be one cursor field", base.hash())
		}
		variants := map[string]listRequest{
			"other parameter value": {col: issues, owner: "octo", repo: "widgets", query: url.Values{"state": {"closed"}, "labels": {"bug"}}, perPage: 50},
			"missing parameter":     {col: issues, owner: "octo", repo: "widgets", query: url.Values{"state": {"open"}}, perPage: 50},
			"extra parameter":       {col: issues, owner: "octo", repo: "widgets", query: url.Values{"state": {"open"}, "labels": {"bug"}, "sort": {"created"}}, perPage: 50},
			"other page size":       {col: issues, owner: "octo", repo: "widgets", query: url.Values{"state": {"open"}, "labels": {"bug"}}, perPage: 100},
			"other collection":      {col: collectionByName("pull_requests"), owner: "octo", repo: "widgets", query: url.Values{"state": {"open"}, "labels": {"bug"}}, perPage: 50},
			"other repository":      {col: issues, owner: "octo", repo: "gadgets", query: url.Values{"state": {"open"}, "labels": {"bug"}}, perPage: 50},
			"other owner":           {col: issues, owner: "hubot", repo: "widgets", query: url.Values{"state": {"open"}, "labels": {"bug"}}, perPage: 50},
		}
		for name, v := range variants {
			if v.hash() == base.hash() {
				t.Errorf("%s: hash did not change", name)
			}
		}
	})
	t.Run("pageQuery", func(t *testing.T) {
		t.Parallel()
		q1 := base.pageQuery(1)
		q2 := base.pageQuery(2)
		want1 := url.Values{"state": {"open"}, "labels": {"bug"}, "per_page": {"50"}, "page": {"1"}}
		want2 := url.Values{"state": {"open"}, "labels": {"bug"}, "per_page": {"50"}, "page": {"2"}}
		if !reflect.DeepEqual(q1, want1) || !reflect.DeepEqual(q2, want2) {
			t.Errorf("pageQuery = %v, %v; want %v, %v", q1, q2, want1, want2)
		}
		if _, ok := base.query["page"]; ok || len(base.query) != 2 {
			t.Errorf("pageQuery changed the base query: %v", base.query)
		}
		q1.Set("state", "closed")
		if base.query.Get("state") != "open" {
			t.Error("a page query shares storage with the base query")
		}
	})
}

// offlineConn is a connection that never contacts the server.
func offlineConn(t *testing.T) *conn {
	t.Helper()
	d := dsnConfig{endpoint: cloudEndpoint, graphql: cloudEndpoint + "/graphql", owner: "octo", repo: "widgets", timeout: defaultTimeout}
	c := newConn(d, driver.ConnConfig{DSN: "github://octo/widgets"})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestPlanBrowse(t *testing.T) {
	t.Parallel()
	c := offlineConn(t)
	t1 := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	cond := func(f string, op driver.Op, v driver.Value) driver.Condition {
		return driver.Condition{Field: f, Op: op, Value: v}
	}
	filter := func(conds ...driver.Condition) driver.BrowseOptions {
		return driver.BrowseOptions{Filter: driver.Filter{Conditions: conds}}
	}
	plan := func(t *testing.T, col string, opts driver.BrowseOptions, limit int) (listRequest, []driver.Condition) {
		t.Helper()
		req, local, err := c.planBrowse(context.Background(), "browse", collectionByName(col), scope{"octo", "widgets"}, opts, limit)
		if err != nil {
			t.Fatalf("planBrowse: %v", err)
		}
		return req, local
	}
	t.Run("pushed conditions", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name  string
			col   string
			conds []driver.Condition
			query url.Values
			local []driver.Condition
		}{
			{"no filter keeps the defaults", "issues", nil, url.Values{"state": {"all"}}, nil},
			{"state eq", "issues", []driver.Condition{cond("state", driver.OpEq, driver.String("open"))}, url.Values{"state": {"open"}}, nil},
			{"labels contains", "issues", []driver.Condition{cond("labels", driver.OpContains, driver.String("bug"))}, url.Values{"state": {"all"}, "labels": {"bug"}}, nil},
			{"assignees is_null", "issues", []driver.Condition{cond("assignees", driver.OpIsNull, driver.Null())}, url.Values{"state": {"all"}, "assignee": {"none"}}, nil},
			{"assignees not_null", "issues", []driver.Condition{cond("assignees", driver.OpNotNull, driver.Null())}, url.Values{"state": {"all"}, "assignee": {"*"}}, nil},
			{"assignees eq", "issues", []driver.Condition{cond("assignees", driver.OpEq, driver.String("octocat"))}, url.Values{"state": {"all"}, "assignee": {"octocat"}}, nil},
			{"updated_at ge", "issues", []driver.Condition{cond("updated_at", driver.OpGe, driver.Time(t1))}, url.Values{"state": {"all"}, "since": {"2024-03-04T05:06:07Z"}}, nil},
			{"user eq", "issues", []driver.Condition{cond("user", driver.OpEq, driver.String("octocat"))}, url.Values{"state": {"all"}, "creator": {"octocat"}}, nil},
			{"milestone is_null", "issues", []driver.Condition{cond("milestone", driver.OpIsNull, driver.Null())}, url.Values{"state": {"all"}, "milestone": {"none"}}, nil},
			{"several pushed", "issues", []driver.Condition{cond("state", driver.OpEq, driver.String("closed")), cond("labels", driver.OpEq, driver.String("bug")), cond("updated_at", driver.OpGe, driver.String("2024-01-01T00:00:00Z"))},
				url.Values{"state": {"closed"}, "labels": {"bug"}, "since": {"2024-01-01T00:00:00Z"}}, nil},
			{"state ne stays local", "issues", []driver.Condition{cond("state", driver.OpNe, driver.String("open"))}, url.Values{"state": {"all"}}, []driver.Condition{cond("state", driver.OpNe, driver.String("open"))}},
			{"updated_at le stays local", "issues", []driver.Condition{cond("updated_at", driver.OpLe, driver.Time(t1))}, url.Values{"state": {"all"}}, []driver.Condition{cond("updated_at", driver.OpLe, driver.Time(t1))}},
			{"comments gt stays local", "issues", []driver.Condition{cond("comments", driver.OpGt, driver.Int(2))}, url.Values{"state": {"all"}}, []driver.Condition{cond("comments", driver.OpGt, driver.Int(2))}},
			{"assignees contains stays local", "issues", []driver.Condition{cond("assignees", driver.OpContains, driver.String("octocat"))}, url.Values{"state": {"all"}}, []driver.Condition{cond("assignees", driver.OpContains, driver.String("octocat"))}},
			{"second label stays local", "issues", []driver.Condition{cond("labels", driver.OpContains, driver.String("bug")), cond("labels", driver.OpContains, driver.String("ui"))},
				url.Values{"state": {"all"}, "labels": {"bug"}}, []driver.Condition{cond("labels", driver.OpContains, driver.String("ui"))}},
			{"pushed and local mixed", "issues", []driver.Condition{cond("title", driver.OpPrefix, driver.String("Fix")), cond("state", driver.OpEq, driver.String("open")), cond("comments", driver.OpGe, driver.Int(1))},
				url.Values{"state": {"open"}}, []driver.Condition{cond("title", driver.OpPrefix, driver.String("Fix")), cond("comments", driver.OpGe, driver.Int(1))}},
			{"pull request head and base", "pull_requests", []driver.Condition{cond("head", driver.OpEq, driver.String("feature")), cond("base", driver.OpEq, driver.String("main"))}, url.Values{"state": {"all"}, "head": {"feature"}, "base": {"main"}}, nil},
			{"commit date range", "commits", []driver.Condition{cond("author_date", driver.OpGe, driver.Time(t1)), cond("committer_date", driver.OpLe, driver.Time(t1.Add(time.Hour)))},
				url.Values{"since": {"2024-03-04T05:06:07Z"}, "until": {"2024-03-04T06:06:07Z"}}, nil},
			{"commit author_date le stays local", "commits", []driver.Condition{cond("author_date", driver.OpLe, driver.Time(t1))}, url.Values{}, []driver.Condition{cond("author_date", driver.OpLe, driver.Time(t1))}},
			{"branch protected", "branches", []driver.Condition{cond("protected", driver.OpEq, driver.Bool(true))}, url.Values{"protected": {"true"}}, nil},
			{"branch protected as text stays local", "branches", []driver.Condition{cond("protected", driver.OpEq, driver.String("true"))}, url.Values{}, []driver.Condition{cond("protected", driver.OpEq, driver.String("true"))}},
			{"workflow run created range", "workflow_runs", []driver.Condition{cond("created_at", driver.OpGe, driver.Time(t1))}, url.Values{"created": {">=2024-03-04T05:06:07Z"}}, nil},
			{"workflow run created lt", "workflow_runs", []driver.Condition{cond("created_at", driver.OpLt, driver.Time(t1))}, url.Values{"created": {"<2024-03-04T05:06:07Z"}}, nil},
			{"workflow run branch", "workflow_runs", []driver.Condition{cond("head_branch", driver.OpEq, driver.String("main"))}, url.Values{"branch": {"main"}}, nil},
			{"workflow status and conclusion share a parameter", "workflow_runs", []driver.Condition{cond("status", driver.OpEq, driver.String("completed")), cond("conclusion", driver.OpEq, driver.String("success"))},
				url.Values{"status": {"completed"}}, []driver.Condition{cond("conclusion", driver.OpEq, driver.String("success"))}},
			{"tags take no parameters", "tags", nil, url.Values{}, nil},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				req, local := plan(t, tc.col, filter(tc.conds...), 100)
				if !reflect.DeepEqual(req.query, tc.query) {
					t.Errorf("query = %v, want %v", req.query, tc.query)
				}
				if len(local) != len(tc.local) || (len(local) > 0 && !reflect.DeepEqual(local, tc.local)) {
					t.Errorf("local = %+v, want %+v", local, tc.local)
				}
				if req.col != collectionByName(tc.col) || req.owner != "octo" || req.repo != "widgets" || req.perPage != 100 {
					t.Errorf("request = %+v", req)
				}
			})
		}
	})
	t.Run("page size", func(t *testing.T) {
		t.Parallel()
		for limit, want := range map[int]int{1: 1, 30: 30, 100: 100, 101: 100, 1000: 100} {
			if req, _ := plan(t, "labels", driver.BrowseOptions{}, limit); req.perPage != want {
				t.Errorf("labels, limit %d: perPage = %d, want %d", limit, req.perPage, want)
			}
			// Pull requests are dropped from an issues page after the read,
			// so issues always read full pages.
			if req, _ := plan(t, "issues", driver.BrowseOptions{}, limit); req.perPage != maxPerPage {
				t.Errorf("issues, limit %d: perPage = %d, want %d", limit, req.perPage, maxPerPage)
			}
		}
	})
	t.Run("sorting", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			col              string
			sort             driver.SortKey
			param, direction string
		}{
			{"issues", driver.SortKey{Field: "created_at", Desc: true}, "created", "desc"},
			{"issues", driver.SortKey{Field: "updated_at"}, "updated", "asc"},
			{"issues", driver.SortKey{Field: "comments", Desc: true}, "comments", "desc"},
			{"pull_requests", driver.SortKey{Field: "created_at"}, "created", "asc"},
			{"issue_comments", driver.SortKey{Field: "updated_at", Desc: true}, "updated", "desc"},
			{"milestones", driver.SortKey{Field: "due_on"}, "due_on", "asc"},
		} {
			req, local := plan(t, tc.col, driver.BrowseOptions{Sort: []driver.SortKey{tc.sort}}, 10)
			if req.query.Get("sort") != tc.param || req.query.Get("direction") != tc.direction || len(local) != 0 {
				t.Errorf("%s sort %+v: query %v, local %v", tc.col, tc.sort, req.query, local)
			}
		}
		req, _ := plan(t, "issues", driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{cond("state", driver.OpEq, driver.String("open"))}}, Sort: []driver.SortKey{{Field: "created_at", Desc: true}}}, 10)
		if want := (url.Values{"state": {"open"}, "sort": {"created"}, "direction": {"desc"}}); !reflect.DeepEqual(req.query, want) {
			t.Errorf("filter and sort = %v, want %v", req.query, want)
		}
	})
	t.Run("refusals", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name string
			col  string
			opts driver.BrowseOptions
			kind error
			hint string
		}{
			{"unknown field", "issues", filter(cond("priority", driver.OpEq, driver.String("high"))), driver.ErrSyntax, `unknown field "priority" in issues`},
			{"field of another collection", "branches", filter(cond("state", driver.OpEq, driver.String("open"))), driver.ErrSyntax, `unknown field "state" in branches`},
			{"unknown field among valid ones", "issues", filter(cond("state", driver.OpEq, driver.String("open")), cond("nope", driver.OpEq, driver.Int(1))), driver.ErrSyntax, `unknown field "nope"`},
			{"raw filter", "issues", driver.BrowseOptions{Filter: driver.Filter{Raw: "state = 'open'"}}, driver.ErrUnsupported, "a raw filter is not supported"},
			{"raw filter beside conditions", "issues", driver.BrowseOptions{Filter: driver.Filter{Raw: "x", Conditions: []driver.Condition{cond("state", driver.OpEq, driver.String("open"))}}}, driver.ErrUnsupported, "a raw filter is not supported"},
			{"two sort keys", "issues", driver.BrowseOptions{Sort: []driver.SortKey{{Field: "created_at"}, {Field: "updated_at"}}}, driver.ErrUnsupported, "one field at a time"},
			{"unsortable field", "issues", driver.BrowseOptions{Sort: []driver.SortKey{{Field: "title"}}}, driver.ErrUnsupported, `cannot sort issues by "title": it sorts by comments, created_at, updated_at`},
			{"unknown sort field", "issues", driver.BrowseOptions{Sort: []driver.SortKey{{Field: "nope"}}}, driver.ErrUnsupported, `cannot sort issues by "nope"`},
			{"collection without sorts", "tags", driver.BrowseOptions{Sort: []driver.SortKey{{Field: "name"}}}, driver.ErrUnsupported, `cannot sort tags by "name": it lists tags in GitHub's own order only`},
			{"commits sorted", "commits", driver.BrowseOptions{Sort: []driver.SortKey{{Field: "author_date", Desc: true}}}, driver.ErrUnsupported, "GitHub's own order only"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				_, _, err := c.planBrowse(context.Background(), "browse", collectionByName(tc.col), scope{"octo", "widgets"}, tc.opts, 100)
				if !errors.Is(err, tc.kind) {
					t.Fatalf("planBrowse = %v, want %v", err, tc.kind)
				}
				if !strings.Contains(err.Error(), tc.hint) {
					t.Errorf("message %q does not mention %q", err, tc.hint)
				}
				var de *driver.Error
				if !errors.As(err, &de) || de.Op != "browse" || de.Driver != Name {
					t.Errorf("attribution = %+v", de)
				}
			})
		}
	})
}

func TestSortableHint(t *testing.T) {
	t.Parallel()
	for col, want := range map[string]string{
		"tags":          ": it lists tags in GitHub's own order only",
		"commits":       ": it lists commits in GitHub's own order only",
		"workflow_runs": ": it lists workflow_runs in GitHub's own order only",
		"issues":        ": it sorts by comments, created_at, updated_at",
		"pull_requests": ": it sorts by created_at, updated_at",
		"milestones":    ": it sorts by due_on",
	} {
		if got := sortableHint(collectionByName(col)); got != want {
			t.Errorf("sortableHint(%s) = %q, want %q", col, got, want)
		}
	}
}
