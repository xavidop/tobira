package github

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestCollections(t *testing.T) {
	t.Parallel()
	writes := map[string][3]bool{ // insert, update, delete
		"issues":         {true, true, false},
		"pull_requests":  {true, true, false},
		"issue_comments": {true, true, true},
		"commits":        {false, false, false},
		"branches":       {false, false, false},
		"tags":           {false, false, false},
		"releases":       {true, true, true},
		"labels":         {true, true, true},
		"milestones":     {true, true, true},
		"workflow_runs":  {false, false, false},
	}
	if len(collections) != len(writes) {
		t.Fatalf("%d collections, want %d: %v", len(collections), len(writes), collectionNames())
	}
	seen := map[string]bool{}
	for _, col := range collections {
		t.Run(col.name, func(t *testing.T) {
			if seen[col.name] {
				t.Errorf("collection %q listed twice", col.name)
			}
			seen[col.name] = true
			if collectionByName(col.name) != col {
				t.Errorf("collectionByName(%q) does not find it", col.name)
			}
			if col.list == nil || col.comment == "" {
				t.Error("a collection needs a list endpoint and a comment")
			}
			if (col.get == nil) != (col.name == "tags") {
				t.Errorf("get endpoint presence = %v; only tags lack one", col.get != nil)
			}
			if (col.envelope != "") != (col.name == "workflow_runs") || (col.name == "workflow_runs" && col.envelope != "workflow_runs") {
				t.Errorf("envelope = %q", col.envelope)
			}
			if col.dropPullRequests != (col.name == "issues") {
				t.Errorf("dropPullRequests = %v", col.dropPullRequests)
			}
			names := map[string]bool{}
			keys := 0
			for _, c := range col.columns {
				if names[c.name] {
					t.Errorf("column %q repeated", c.name)
				}
				names[c.name] = true
				if c.key {
					keys++
				}
				if (c.path == "") == (c.conv == nil) {
					t.Errorf("column %q needs exactly one of a path or a reader", c.name)
				}
				if c.typ == driver.TypeNull || c.typ == driver.TypeUnknown || c.native == "" {
					t.Errorf("column %q has type %s and native %q", c.name, c.typ, c.native)
				}
			}
			if !names[col.keyCol] {
				t.Errorf("key column %q is not a column", col.keyCol)
			}
			if keys != 1 || !col.col(col.keyCol).key {
				t.Errorf("%d key columns; want only %q", keys, col.keyCol)
			}
			for f, pd := range col.filters {
				if !names[f] {
					t.Errorf("filter %q is not a column", f)
				}
				if pd.param == "" || pd.encode == nil {
					t.Errorf("filter %q has no parameter or encoder", f)
				}
			}
			for f, param := range col.sorts {
				if !names[f] || param == "" {
					t.Errorf("sort %q -> %q is not a column with a parameter", f, param)
				}
			}
			for k := range col.defaults {
				found := false
				for _, p := range col.params {
					found = found || p == k
				}
				if !found {
					t.Errorf("default %q is not a list parameter", k)
				}
			}
			want := writes[col.name]
			for i, m := range []*mutation{col.insert, col.update, col.delete} {
				if (m != nil) != want[i] {
					t.Errorf("mutation %d present = %v, want %v", i, m != nil, want[i])
				}
				if m == nil {
					continue
				}
				if m.path == nil || m.method == "" {
					t.Errorf("mutation %d has no path or method", i)
				}
				if i == 2 && len(m.fields) != 0 {
					t.Errorf("delete carries fields %v", m.fields)
				}
				for _, f := range m.fields {
					c := col.col(f)
					if c == nil {
						t.Errorf("mutation %d field %q is not a column", i, f)
					} else if !c.writable {
						t.Errorf("mutation %d field %q is not writable", i, f)
					}
				}
			}
			if col.insert != nil && col.insert.method != http.MethodPost {
				t.Errorf("insert method = %s", col.insert.method)
			}
			if col.update != nil && col.update.method != http.MethodPatch {
				t.Errorf("update method = %s", col.update.method)
			}
			if col.delete != nil && col.delete.method != http.MethodDelete {
				t.Errorf("delete method = %s", col.delete.method)
			}
			// Every required column is writable, so an insert can give it.
			for _, c := range col.columns {
				if c.required && !c.writable {
					t.Errorf("required column %q is not writable", c.name)
				}
				if c.required && col.insert != nil {
					found := false
					for _, f := range col.insert.fields {
						found = found || f == c.name
					}
					if !found {
						t.Errorf("required column %q is not an insert field", c.name)
					}
				}
			}
		})
	}
	if collectionByName("gists") != nil || collectionByName("") != nil {
		t.Error("an unknown collection was found")
	}
	if names := collectionNames(); len(names) != len(collections) || names[0] != "issues" || names[len(names)-1] != "workflow_runs" {
		t.Errorf("collectionNames = %v", names)
	}
}

func TestCollectionPaths(t *testing.T) {
	t.Parallel()
	issues := collectionByName("issues")
	if got := issues.list("octo", "widgets"); got != "/repos/octo/widgets/issues" {
		t.Errorf("issues list = %q", got)
	}
	if got := issues.get("octo", "widgets", "12"); got != "/repos/octo/widgets/issues/12" {
		t.Errorf("issues get = %q", got)
	}
	if got := issues.insert.path("octo", "widgets", "ignored"); got != "/repos/octo/widgets/issues" {
		t.Errorf("issues insert path = %q", got)
	}
	if got := issues.update.path("octo", "widgets", "12"); got != "/repos/octo/widgets/issues/12" {
		t.Errorf("issues update path = %q", got)
	}
	labels := collectionByName("labels")
	if got := labels.get("octo", "widgets", "help wanted"); got != "/repos/octo/widgets/labels/help%20wanted" {
		t.Errorf("labels get = %q", got)
	}
	if got := labels.delete.path("octo", "widgets", "a/b"); got != "/repos/octo/widgets/labels/a%2Fb" {
		t.Errorf("labels delete path = %q", got)
	}
	comments := collectionByName("issue_comments")
	if got := comments.insert.path("octo", "widgets", "42"); got != "/repos/octo/widgets/issues/42/comments" {
		t.Errorf("comment insert path = %q", got)
	}
	if got := comments.list("octo", "widgets"); got != "/repos/octo/widgets/issues/comments" {
		t.Errorf("comments list = %q", got)
	}
	if got := collectionByName("workflow_runs").list("octo", "widgets"); got != "/repos/octo/widgets/actions/runs" {
		t.Errorf("workflow runs list = %q", got)
	}
	if got := collectionByName("pull_requests").list("octo", "widgets"); got != "/repos/octo/widgets/pulls" {
		t.Errorf("pulls list = %q", got)
	}
}

func TestFields(t *testing.T) {
	t.Parallel()
	field := func(t *testing.T, col, name string) driver.Field {
		t.Helper()
		for _, f := range collectionByName(col).fields() {
			if f.Name == name {
				return f
			}
		}
		t.Fatalf("%s has no field %q", col, name)
		return driver.Field{}
	}
	tests := []struct {
		col, name                          string
		generated, autoIncrement, nullable bool
	}{
		{"issues", "number", false, true, false}, // GitHub assigns the number
		{"issues", "title", false, false, false}, // required
		{"issues", "state", false, false, true},  // optional, writable
		{"issues", "user", true, false, true},    // computed
		{"issues", "created_at", true, false, true},
		{"issues", "labels", false, false, true},
		{"pull_requests", "number", false, true, false},
		{"pull_requests", "head", false, false, false},
		{"pull_requests", "merged_at", true, false, true},
		{"issue_comments", "id", false, true, false},
		{"issue_comments", "issue", false, false, false},
		{"issue_comments", "body", false, false, false},
		{"releases", "id", false, true, false},
		{"releases", "tag_name", false, false, false},
		{"releases", "assets", true, false, true},
		{"labels", "name", false, false, false}, // the caller names a label
		{"labels", "color", false, false, true},
		{"labels", "id", true, false, true},
		{"milestones", "number", false, true, false},
		{"milestones", "due_on", false, false, true},
		{"commits", "sha", true, false, false}, // read-only: no insert, so the key is just computed
		{"commits", "message", true, false, true},
		{"branches", "name", true, false, false},
		{"tags", "name", true, false, false},
		{"workflow_runs", "id", true, false, false},
	}
	for _, tc := range tests {
		f := field(t, tc.col, tc.name)
		if f.Generated != tc.generated || f.AutoIncrement != tc.autoIncrement || f.Nullable != tc.nullable {
			t.Errorf("%s.%s: generated %v, auto %v, nullable %v; want %v, %v, %v", tc.col, tc.name, f.Generated, f.AutoIncrement, f.Nullable, tc.generated, tc.autoIncrement, tc.nullable)
		}
	}
	for _, col := range collections {
		fields := col.fields()
		if len(fields) != len(col.columns) {
			t.Errorf("%s: %d fields for %d columns", col.name, len(fields), len(col.columns))
		}
		for i, f := range fields {
			c := col.columns[i]
			if f.Name != c.name || f.Type != c.typ || f.NativeType != c.native || f.Comment != c.comment {
				t.Errorf("%s.%s: field %+v does not mirror the column", col.name, c.name, f)
			}
			if f.Generated && f.AutoIncrement {
				t.Errorf("%s.%s is both Generated and AutoIncrement", col.name, c.name)
			}
			if c.key && f.Nullable {
				t.Errorf("%s.%s: the key is Nullable", col.name, c.name)
			}
		}
		ref := driver.CollectionRef{Namespace: "octo/widgets", Name: col.name}
		cols := col.resultColumns(ref)
		for i, rc := range cols {
			c := col.columns[i]
			if rc.Name != c.name || rc.Type != c.typ || rc.NativeType != c.native || rc.Key != c.key || rc.Source == nil || *rc.Source != ref {
				t.Errorf("%s.%s: result column %+v", col.name, c.name, rc)
			}
		}
	}
}

const issueJSON = `{
	"id": 1,
	"number": 1347,
	"title": "Found a bug",
	"state": "open",
	"state_reason": null,
	"user": {"login": "octocat", "id": 1, "type": "User"},
	"labels": [{"id": 208045946, "name": "bug", "color": "f29513"}, {"id": 2, "name": "help wanted"}],
	"assignees": [{"login": "octocat"}, {"login": "hubot"}],
	"milestone": {"number": 1, "title": "v1.0", "state": "open"},
	"comments": 3,
	"locked": false,
	"author_association": "OWNER",
	"created_at": "2011-04-22T13:33:48Z",
	"updated_at": "2011-04-23T13:33:48Z",
	"closed_at": null,
	"body": "I'm having a problem with this.",
	"html_url": "https://github.com/octocat/Hello-World/issues/1347"
}`

func TestRow(t *testing.T) {
	t.Parallel()
	cell := func(t *testing.T, cols []column, row driver.Row, name string) driver.Value {
		t.Helper()
		i := indexOf(cols, name)
		if i < 0 {
			t.Fatalf("no column %q", name)
		}
		return row[i]
	}
	t.Run("issue", func(t *testing.T) {
		t.Parallel()
		col := collectionByName("issues")
		row, err := col.row(json.RawMessage(issueJSON))
		if err != nil {
			t.Fatalf("row: %v", err)
		}
		if len(row) != len(col.columns) {
			t.Fatalf("%d cells for %d columns", len(row), len(col.columns))
		}
		want := map[string]driver.Value{
			"number":             driver.Int(1347),
			"title":              driver.String("Found a bug"),
			"state":              driver.String("open"),
			"state_reason":       driver.Null(),
			"user":               driver.String("octocat"),
			"labels":             driver.List(driver.String("bug"), driver.String("help wanted")),
			"assignees":          driver.List(driver.String("octocat"), driver.String("hubot")),
			"milestone":          driver.String("v1.0"),
			"comments":           driver.Int(3),
			"locked":             driver.Bool(false),
			"author_association": driver.String("OWNER"),
			"created_at":         driver.Time(time.Date(2011, 4, 22, 13, 33, 48, 0, time.UTC)),
			"updated_at":         driver.Time(time.Date(2011, 4, 23, 13, 33, 48, 0, time.UTC)),
			"closed_at":          driver.Null(),
			"body":               driver.String("I'm having a problem with this."),
			"html_url":           driver.String("https://github.com/octocat/Hello-World/issues/1347"),
		}
		for name, v := range want {
			assertValue(t, name, cell(t, col.columns, row, name), v)
		}
	})
	t.Run("sparse issue", func(t *testing.T) {
		t.Parallel()
		col := collectionByName("issues")
		row, err := col.row(json.RawMessage(`{"number": 2, "title": "t", "user": null, "labels": [{"id": 1}, {"name": "bug"}], "assignees": [], "milestone": null, "comments": 0}`))
		if err != nil {
			t.Fatalf("row: %v", err)
		}
		assertValue(t, "user", cell(t, col.columns, row, "user"), driver.Null())
		assertValue(t, "labels", cell(t, col.columns, row, "labels"), driver.List(driver.String("bug")))
		assertValue(t, "assignees", cell(t, col.columns, row, "assignees"), driver.List())
		assertValue(t, "milestone", cell(t, col.columns, row, "milestone"), driver.Null())
		assertValue(t, "created_at", cell(t, col.columns, row, "created_at"), driver.Null())
		assertValue(t, "locked", cell(t, col.columns, row, "locked"), driver.Null())
		// An unexpected shape is kept as JSON rather than dropped.
		row, err = col.row(json.RawMessage(`{"number": "3", "comments": "many", "labels": "none", "created_at": "soon", "assignees": {"login": "x"}}`))
		if err != nil {
			t.Fatalf("row: %v", err)
		}
		assertValue(t, "number", cell(t, col.columns, row, "number"), driver.Int(3))
		assertValue(t, "comments", cell(t, col.columns, row, "comments"), driver.JSON([]byte(`"many"`)))
		assertValue(t, "labels", cell(t, col.columns, row, "labels"), driver.JSON([]byte(`"none"`)))
		assertValue(t, "created_at", cell(t, col.columns, row, "created_at"), driver.String("soon"))
		assertValue(t, "assignees", cell(t, col.columns, row, "assignees"), driver.JSON([]byte(`{"login": "x"}`)))
	})
	t.Run("commit", func(t *testing.T) {
		t.Parallel()
		col := collectionByName("commits")
		row, err := col.row(json.RawMessage(`{
			"sha": "6dcb09b5b57875f334f61aebed695e2e4193db5e",
			"commit": {"message": "Fix all the bugs", "author": {"name": "Monalisa", "email": "m@github.com", "date": "2011-04-14T16:00:49Z"}, "committer": {"name": "GitHub", "date": "2011-04-14T16:00:50Z"}},
			"author": null,
			"committer": {"login": "web-flow"},
			"parents": [{"sha": "p1"}, {"sha": "p2"}],
			"html_url": "https://github.com/octocat/Hello-World/commit/6dcb09b"
		}`))
		if err != nil {
			t.Fatalf("row: %v", err)
		}
		want := map[string]driver.Value{
			"sha":            driver.String("6dcb09b5b57875f334f61aebed695e2e4193db5e"),
			"message":        driver.String("Fix all the bugs"),
			"author":         driver.Null(),
			"author_name":    driver.String("Monalisa"),
			"author_email":   driver.String("m@github.com"),
			"author_date":    driver.Time(time.Date(2011, 4, 14, 16, 0, 49, 0, time.UTC)),
			"committer":      driver.String("web-flow"),
			"committer_name": driver.String("GitHub"),
			"committer_date": driver.Time(time.Date(2011, 4, 14, 16, 0, 50, 0, time.UTC)),
			"parents":        driver.List(driver.String("p1"), driver.String("p2")),
		}
		for name, v := range want {
			assertValue(t, name, cell(t, col.columns, row, name), v)
		}
	})
	t.Run("comment", func(t *testing.T) {
		t.Parallel()
		col := collectionByName("issue_comments")
		row, err := col.row(json.RawMessage(`{"id": 5, "issue_url": "https://api.github.com/repos/octo/widgets/issues/42", "user": {"login": "hubot"}, "body": "hi"}`))
		if err != nil {
			t.Fatalf("row: %v", err)
		}
		assertValue(t, "id", cell(t, col.columns, row, "id"), driver.Int(5))
		assertValue(t, "issue", cell(t, col.columns, row, "issue"), driver.Int(42))
		assertValue(t, "user", cell(t, col.columns, row, "user"), driver.String("hubot"))
		row, err = col.row(json.RawMessage(`{"id": 6, "issue_url": "https://api.github.com/repos/octo/widgets/issues/latest"}`))
		if err != nil {
			t.Fatalf("row: %v", err)
		}
		assertValue(t, "issue without a number", cell(t, col.columns, row, "issue"), driver.String("https://api.github.com/repos/octo/widgets/issues/latest"))
		row, _ = col.row(json.RawMessage(`{"id": 7, "issue_url": 12}`))
		assertValue(t, "issue_url not text", cell(t, col.columns, row, "issue"), driver.Null())
	})
	t.Run("release", func(t *testing.T) {
		t.Parallel()
		col := collectionByName("releases")
		for raw, want := range map[string]driver.Value{
			`{"id": 1, "assets": [{"id": 1}, {"id": 2}]}`: driver.Int(2),
			`{"id": 1, "assets": []}`:                     driver.Int(0),
			`{"id": 1}`:                                   driver.Int(0),
			`{"id": 1, "assets": null}`:                   driver.Int(0),
			`{"id": 1, "assets": 5}`:                      driver.Null(),
		} {
			row, err := col.row(json.RawMessage(raw))
			if err != nil {
				t.Fatalf("row(%s): %v", raw, err)
			}
			assertValue(t, "assets of "+raw, cell(t, col.columns, row, "assets"), want)
		}
	})
	t.Run("repository", func(t *testing.T) {
		t.Parallel()
		row, err := rowFrom(repositoryColumns, json.RawMessage(`{"full_name": "octo/widgets", "owner": {"login": "octo"}, "name": "widgets", "private": true, "topics": ["cli", "go"], "stargazers_count": 42, "pushed_at": "2024-01-02T03:04:05Z"}`))
		if err != nil {
			t.Fatalf("rowFrom: %v", err)
		}
		assertValue(t, "full_name", cell(t, repositoryColumns, row, "full_name"), driver.String("octo/widgets"))
		assertValue(t, "owner", cell(t, repositoryColumns, row, "owner"), driver.String("octo"))
		assertValue(t, "private", cell(t, repositoryColumns, row, "private"), driver.Bool(true))
		assertValue(t, "topics", cell(t, repositoryColumns, row, "topics"), driver.List(driver.String("cli"), driver.String("go")))
		assertValue(t, "stars", cell(t, repositoryColumns, row, "stars"), driver.Int(42))
		assertValue(t, "pushed_at", cell(t, repositoryColumns, row, "pushed_at"), driver.Time(time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)))
		assertValue(t, "language", cell(t, repositoryColumns, row, "language"), driver.Null())
	})
	t.Run("search issue", func(t *testing.T) {
		t.Parallel()
		cols := searchColumns("issues")
		row, err := rowFrom(cols, json.RawMessage(`{"number": 1, "pull_request": {"url": "x"}, "repository_url": "https://api.github.com/repos/octo/widgets"}`))
		if err != nil {
			t.Fatalf("rowFrom: %v", err)
		}
		assertValue(t, "pull_request", cell(t, cols, row, "pull_request"), driver.Bool(true))
		assertValue(t, "repository", cell(t, cols, row, "repository"), driver.String("octo/widgets"))
		row, _ = rowFrom(cols, json.RawMessage(`{"number": 2, "pull_request": null, "repository_url": "weird"}`))
		assertValue(t, "pull_request absent", cell(t, cols, row, "pull_request"), driver.Bool(false))
		assertValue(t, "repository from odd url", cell(t, cols, row, "repository"), driver.String("weird"))
		row, _ = rowFrom(cols, json.RawMessage(`{"number": 3}`))
		assertValue(t, "repository missing", cell(t, cols, row, "repository"), driver.Null())
		row, _ = rowFrom(cols, json.RawMessage(`{"number": 3, "repository_url": 7}`))
		assertValue(t, "repository_url not text", cell(t, cols, row, "repository"), driver.Null())
	})
	t.Run("search commit", func(t *testing.T) {
		t.Parallel()
		cols := searchColumns("commits")
		row, err := rowFrom(cols, json.RawMessage(`{"sha": "abc", "repository": {"full_name": "octo/widgets"}}`))
		if err != nil {
			t.Fatalf("rowFrom: %v", err)
		}
		assertValue(t, "repository", cell(t, cols, row, "repository"), driver.String("octo/widgets"))
	})
	t.Run("not an object", func(t *testing.T) {
		t.Parallel()
		col := collectionByName("issues")
		for _, raw := range []string{`[1]`, `null`, `"x"`, `{bad`} {
			if row, err := col.row(json.RawMessage(raw)); err == nil {
				t.Errorf("row(%s) = %v, want an error", raw, row)
			}
		}
	})
}

func TestIsPullRequest(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]bool{
		`{"number": 1, "pull_request": {"url": "https://api.github.com/repos/o/r/pulls/1"}}`: true,
		`{"number": 1, "pull_request": {}}`:                                                  true,
		`{"number": 1, "pull_request": null}`:                                                false,
		`{"number": 1}`:                                                                      false,
		`[1]`:                                                                                false,
		`null`:                                                                               false,
		`{bad`:                                                                               false,
	} {
		if got := isPullRequest(json.RawMessage(raw)); got != want {
			t.Errorf("isPullRequest(%s) = %v, want %v", raw, got, want)
		}
	}
}

func TestSearchColumns(t *testing.T) {
	t.Parallel()
	issueCount := len(issueColumns)
	cols := searchColumns("issues")
	if len(cols) != issueCount+2 || cols[issueCount].name != "pull_request" || cols[issueCount].typ != driver.TypeBool || cols[issueCount+1].name != "repository" {
		t.Errorf("issues search columns = %d, tail %q, %q", len(cols), cols[len(cols)-2].name, cols[len(cols)-1].name)
	}
	if len(issueColumns) != issueCount {
		t.Error("searchColumns grew issueColumns")
	}
	for i, c := range issueColumns {
		if cols[i].name != c.name {
			t.Errorf("issues search column %d = %q, want %q", i, cols[i].name, c.name)
		}
	}
	commitCount := len(commitColumns)
	cols = searchColumns("commits")
	if len(cols) != commitCount+1 || cols[commitCount].name != "repository" || cols[commitCount].path != "repository.full_name" {
		t.Errorf("commits search columns = %d, tail %+v", len(cols), cols[len(cols)-1])
	}
	if len(commitColumns) != commitCount {
		t.Error("searchColumns grew commitColumns")
	}
	cols = searchColumns("repositories")
	if len(cols) != len(repositoryColumns) || cols[0].name != "full_name" || !cols[0].key {
		t.Errorf("repositories search columns = %+v", cols)
	}
	for _, typ := range []string{"pull_requests", "users", ""} {
		if got := searchColumns(typ); got != nil {
			t.Errorf("searchColumns(%q) = %v, want none", typ, got)
		}
	}
	// Every search type a query may name has columns, pull requests through issues.
	for _, typ := range searchTypes {
		if typ == "pull_requests" {
			typ = "issues"
		}
		if searchColumns(typ) == nil {
			t.Errorf("search type %q has no columns", typ)
		}
	}
}

func TestPushdown(t *testing.T) {
	t.Parallel()
	t1 := time.Date(2024, 3, 4, 7, 6, 7, 0, time.FixedZone("x", 2*3600)) // 05:06:07Z
	cond := func(op driver.Op, v driver.Value) driver.Condition {
		return driver.Condition{Field: "f", Op: op, Value: v}
	}
	tests := []struct {
		name string
		pd   pushdown
		cond driver.Condition
		want string
		ok   bool
	}{
		{"eqText eq", eqText("state"), cond(driver.OpEq, driver.String("open")), "open", true},
		{"eqText eq number", eqText("state"), cond(driver.OpEq, driver.Int(1)), "1", true},
		{"eqText ne", eqText("state"), cond(driver.OpNe, driver.String("open")), "", false},
		{"eqText contains", eqText("state"), cond(driver.OpContains, driver.String("op")), "", false},
		{"eqText bool", eqText("state"), cond(driver.OpEq, driver.Bool(true)), "", false},
		{"eqText is_null", eqText("state"), cond(driver.OpIsNull, driver.Null()), "", false},
		{"eqTextOrNone eq", eqTextOrNone("assignee"), cond(driver.OpEq, driver.String("octocat")), "octocat", true},
		{"eqTextOrNone is_null", eqTextOrNone("assignee"), cond(driver.OpIsNull, driver.Null()), "none", true},
		{"eqTextOrNone not_null", eqTextOrNone("assignee"), cond(driver.OpNotNull, driver.Null()), "*", true},
		{"eqTextOrNone ne", eqTextOrNone("assignee"), cond(driver.OpNe, driver.String("octocat")), "", false},
		{"eqTextOrNone eq list", eqTextOrNone("assignee"), cond(driver.OpEq, driver.List(driver.String("a"))), "", false},
		{"containsLabel contains", containsLabel("labels"), cond(driver.OpContains, driver.String("bug")), "bug", true},
		{"containsLabel eq", containsLabel("labels"), cond(driver.OpEq, driver.String("bug")), "bug", true},
		{"containsLabel in", containsLabel("labels"), cond(driver.OpIn, driver.List(driver.String("bug"))), "", false},
		{"containsLabel ne", containsLabel("labels"), cond(driver.OpNe, driver.String("bug")), "", false},
		{"containsLabel contains list", containsLabel("labels"), cond(driver.OpContains, driver.List(driver.String("bug"))), "", false},
		{"sinceTime ge time", sinceTime("since"), cond(driver.OpGe, driver.Time(t1)), "2024-03-04T05:06:07Z", true},
		{"sinceTime ge text", sinceTime("since"), cond(driver.OpGe, driver.String("2024-03-04T00:00:00Z")), "2024-03-04T00:00:00Z", true},
		{"sinceTime gt", sinceTime("since"), cond(driver.OpGt, driver.Time(t1)), "", false},
		{"sinceTime le", sinceTime("since"), cond(driver.OpLe, driver.Time(t1)), "", false},
		{"sinceTime eq", sinceTime("since"), cond(driver.OpEq, driver.Time(t1)), "", false},
		{"sinceTime ge number", sinceTime("since"), cond(driver.OpGe, driver.Int(1)), "", false},
		{"sinceTime ge empty text", sinceTime("since"), cond(driver.OpGe, driver.String("")), "", false},
		{"untilTime le time", untilTime("until"), cond(driver.OpLe, driver.Time(t1)), "2024-03-04T05:06:07Z", true},
		{"untilTime le text", untilTime("until"), cond(driver.OpLe, driver.String("2024-03-05")), "2024-03-05", true},
		{"untilTime lt", untilTime("until"), cond(driver.OpLt, driver.Time(t1)), "", false},
		{"untilTime ge", untilTime("until"), cond(driver.OpGe, driver.Time(t1)), "", false},
		{"createdRange ge", createdRange("created"), cond(driver.OpGe, driver.Time(t1)), ">=2024-03-04T05:06:07Z", true},
		{"createdRange le", createdRange("created"), cond(driver.OpLe, driver.Time(t1)), "<=2024-03-04T05:06:07Z", true},
		{"createdRange gt", createdRange("created"), cond(driver.OpGt, driver.Time(t1)), ">2024-03-04T05:06:07Z", true},
		{"createdRange lt text", createdRange("created"), cond(driver.OpLt, driver.String("2024-03-04")), "<2024-03-04", true},
		{"createdRange eq", createdRange("created"), cond(driver.OpEq, driver.Time(t1)), "", false},
		{"createdRange ne", createdRange("created"), cond(driver.OpNe, driver.Time(t1)), "", false},
		{"createdRange ge bool", createdRange("created"), cond(driver.OpGe, driver.Bool(true)), "", false},
		{"eqBool true", eqBool("protected"), cond(driver.OpEq, driver.Bool(true)), "true", true},
		{"eqBool false", eqBool("protected"), cond(driver.OpEq, driver.Bool(false)), "false", true},
		{"eqBool text", eqBool("protected"), cond(driver.OpEq, driver.String("true")), "", false},
		{"eqBool ne", eqBool("protected"), cond(driver.OpNe, driver.Bool(true)), "", false},
	}
	for _, tc := range tests {
		got, ok := tc.pd.encode(tc.cond)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: encode = %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
	for _, tc := range []struct {
		pd    pushdown
		param string
	}{
		{eqText("state"), "state"}, {eqTextOrNone("assignee"), "assignee"}, {containsLabel("labels"), "labels"},
		{sinceTime("since"), "since"}, {untilTime("until"), "until"}, {createdRange("created"), "created"}, {eqBool("protected"), "protected"},
	} {
		if tc.pd.param != tc.param {
			t.Errorf("param = %q, want %q", tc.pd.param, tc.param)
		}
	}
	for _, tc := range []struct {
		v    driver.Value
		want string
	}{
		{driver.Time(t1), "2024-03-04T05:06:07Z"},
		{driver.Time(t1.Add(500 * time.Millisecond)), "2024-03-04T05:06:07Z"},
		{driver.String("2024-03-04"), "2024-03-04"},
		{driver.Decimal("1"), "1"},
	} {
		if got, ok := timeText(tc.v); !ok || got != tc.want {
			t.Errorf("timeText(%v) = %q, %v; want %q", tc.v, got, ok, tc.want)
		}
	}
	for _, v := range []driver.Value{driver.String(""), driver.Int(1), driver.Bool(true), driver.Null(), driver.List(driver.String("x"))} {
		if got, ok := timeText(v); ok {
			t.Errorf("timeText(%v) = %q, want refused", v, got)
		}
	}
}
