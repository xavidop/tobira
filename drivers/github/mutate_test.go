package github

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

// Insert creates an issue (GitHub assigns the number) and a get reads it
// back, labels included.
func TestInsertIssueAndReadItBack(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	ref := driver.CollectionRef{Namespace: "octo/widgets", Name: "issues"}
	key, err := c.Insert(context.Background(), ref, driver.Object{
		{Key: "title", Value: driver.String("Widgets need a handle")},
		{Key: "body", Value: driver.String("They roll away.")},
		{Key: "labels", Value: driver.List(driver.String("bug"), driver.String("triage"))},
		{Key: "milestone", Value: driver.String("v2.0")},
	})
	if err != nil {
		t.Fatal(err)
	}
	n, ok := key.Get("number")
	if !ok || n.Type() != driver.TypeInt {
		t.Fatalf("Insert returned %+v, want the number", key)
	}
	num, _ := n.AsInt()
	writes := f.writes()
	if len(writes) != 1 || writes[0].Method != "POST" || writes[0].Path != "/repos/octo/widgets/issues" {
		t.Fatalf("writes = %+v", writes)
	}
	var sent map[string]any
	if err := json.Unmarshal(writes[0].Body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["milestone"] != 2.0 || sent["title"] != "Widgets need a handle" {
		t.Errorf("the body sent = %v, want the milestone resolved to its number", sent)
	}
	cols, rows, _ := queryText(t, c, `{"op":"get","collection":"issues","number":`+n.String()+`}`)
	if len(rows) != 1 {
		t.Fatalf("get of the new issue: %d rows", len(rows))
	}
	if title, _ := cell(t, cols, rows[0], "title").AsText(); title != "Widgets need a handle" {
		t.Errorf("title = %q", title)
	}
	if ms, _ := cell(t, cols, rows[0], "milestone").AsText(); ms != "v2.0" {
		t.Errorf("milestone = %q", ms)
	}
	labels, _ := cell(t, cols, rows[0], "labels").AsList()
	if len(labels) != 2 {
		t.Errorf("labels = %v", labels)
	}
	if got, _ := cell(t, cols, rows[0], "number").AsInt(); got != num {
		t.Errorf("number = %d, want %d", got, num)
	}
}

// Update renames a label through new_name and changes its colour.
func TestUpdateLabelRenames(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	ref := driver.CollectionRef{Namespace: "octo/widgets", Name: "labels"}
	err := c.Update(context.Background(), ref, driver.Object{{Key: "name", Value: driver.String("wontfix")}}, driver.Object{
		{Key: "name", Value: driver.String("will-not-fix")},
		{Key: "color", Value: driver.String("000000")},
	})
	if err != nil {
		t.Fatal(err)
	}
	writes := f.writes()
	if len(writes) != 1 || writes[0].Method != "PATCH" || writes[0].Path != "/repos/octo/widgets/labels/wontfix" {
		t.Fatalf("writes = %+v", writes)
	}
	var sent map[string]any
	_ = json.Unmarshal(writes[0].Body, &sent)
	if sent["new_name"] != "will-not-fix" || sent["color"] != "000000" || sent["name"] != nil {
		t.Errorf("the body sent = %v, want new_name", sent)
	}
	if f.find("octo", "widgets", "labels", func(o object) bool { return str(o, "name") == "wontfix" }) != nil {
		t.Error("the old label name is still there")
	}
	l := f.find("octo", "widgets", "labels", func(o object) bool { return str(o, "name") == "will-not-fix" })
	if l == nil || str(l, "color") != "000000" {
		t.Errorf("renamed label = %v", l)
	}
	err = c.Update(context.Background(), ref, driver.Object{{Key: "name", Value: driver.String("nothing")}}, driver.Object{{Key: "color", Value: driver.String("ffffff")}})
	wantKind(t, "Update of a missing label", err, driver.ErrNotFound)
}

// Delete removes a release; GitHub answers 204.
func TestDeleteRelease(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	rel := f.find("octo", "widgets", "releases", func(o object) bool { return str(o, "tag_name") == "v0.9.0" })
	if rel == nil {
		t.Fatal("no seeded release v0.9.0")
	}
	before := f.count("octo", "widgets", "releases")
	ref := driver.CollectionRef{Namespace: "octo/widgets", Name: "releases"}
	if err := c.Delete(context.Background(), ref, driver.Object{{Key: "id", Value: driver.Int(num(rel, "id"))}}); err != nil {
		t.Fatal(err)
	}
	writes := f.writes()
	if len(writes) != 1 || writes[0].Method != "DELETE" || writes[0].Status != 204 {
		t.Fatalf("writes = %+v", writes)
	}
	if after := f.count("octo", "widgets", "releases"); after != before-1 {
		t.Errorf("releases after delete: %d, want %d", after, before-1)
	}
	err := c.Delete(context.Background(), ref, driver.Object{{Key: "id", Value: driver.Int(num(rel, "id"))}})
	wantKind(t, "Delete of a deleted release", err, driver.ErrNotFound)
}

// Issues cannot be deleted through the REST API; the error says to close
// them. Collections without writes are ErrUnsupported.
func TestDeleteIssueIsUnsupported(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.resetLog()
	err := c.Delete(context.Background(), driver.CollectionRef{Namespace: "octo/widgets", Name: "issues"}, driver.Object{{Key: "number", Value: driver.Int(1)}})
	wantKind(t, "Delete of an issue", err, driver.ErrUnsupported)
	_, err = c.Insert(context.Background(), driver.CollectionRef{Namespace: "octo/widgets", Name: "commits"}, driver.Object{{Key: "message", Value: driver.String("x")}})
	wantKind(t, "Insert of a commit", err, driver.ErrUnsupported)
	if w := f.writes(); len(w) != 0 {
		t.Errorf("unsupported writes reached the server: %+v", w)
	}
}

// sentBody decodes the JSON body of a recorded request.
func sentBody(t *testing.T, r fakeRequest) map[string]any {
	t.Helper()
	var body map[string]any
	if len(r.Body) > 0 {
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatalf("the body sent is not JSON: %v", err)
		}
	}
	return body
}

// lastWrite is the most recent write the fake received.
func lastWrite(t *testing.T, f *fakeGitHub) fakeRequest {
	t.Helper()
	writes := f.writes()
	if len(writes) == 0 {
		t.Fatal("no write reached the server")
	}
	return writes[len(writes)-1]
}

func ref(name string) driver.CollectionRef {
	return driver.CollectionRef{Namespace: "octo/widgets", Name: name}
}

// getRow reads one item back by key through a get query.
func getRow(t *testing.T, c *conn, col string, key driver.Value) ([]driver.Column, driver.Row) {
	t.Helper()
	keyCol := collectionByName(col).keyCol
	k, _ := key.AsText()
	if key.Type() != driver.TypeString {
		k = key.String()
	}
	cols, rows, _ := queryText(t, c, fmt.Sprintf(`{"op":"get","collection":"%s","%s":%q}`, col, keyCol, k))
	if len(rows) != 1 {
		t.Fatalf("get %s %v: %d rows", col, key, len(rows))
	}
	return cols, rows[0]
}

func textList(t *testing.T, v driver.Value) []string {
	t.Helper()
	items, ok := v.AsList()
	if !ok {
		t.Fatalf("%v is not a list", v)
	}
	out := make([]string, len(items))
	for i, it := range items {
		out[i], _ = it.AsText()
	}
	return out
}

// Every collection GitHub creates through tobira is inserted and read back
// by the key the insert returned, with each accepted spelling of the
// members: labels as a list, a JSON array or comma-separated text, the
// milestone by title or by number, the comment's issue as a number or
// text, a due date as a time or RFC 3339 text.
func TestInsertEachCollectionAndReadItBack(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	ctx := context.Background()
	insert := func(t *testing.T, col string, values driver.Object) driver.Value {
		t.Helper()
		f.resetLog()
		key, err := c.Insert(ctx, ref(col), values)
		if err != nil {
			t.Fatalf("Insert %s: %v", col, err)
		}
		v, ok := key.Get(collectionByName(col).keyCol)
		if !ok || len(key) != 1 {
			t.Fatalf("Insert %s returned %+v, want the %s", col, key, collectionByName(col).keyCol)
		}
		return v
	}
	t.Run("issues with labels as a JSON array and a milestone by number", func(t *testing.T) {
		key := insert(t, "issues", driver.Object{
			{Key: "title", Value: driver.String("Wobbly widget")},
			{Key: "labels", Value: driver.JSON([]byte(`["bug","question"]`))},
			{Key: "assignees", Value: driver.List(driver.String("hubot"))},
			{Key: "milestone", Value: driver.Int(1)},
		})
		sent := sentBody(t, lastWrite(t, f))
		if !reflect.DeepEqual(sent["labels"], []any{"bug", "question"}) || !reflect.DeepEqual(sent["assignees"], []any{"hubot"}) || sent["milestone"] != 1.0 {
			t.Errorf("body sent = %v", sent)
		}
		if n := len(f.log()); n != 1 {
			t.Errorf("a milestone given by number sent %d requests, want the POST alone", n)
		}
		cols, row := getRow(t, c, "issues", key)
		if ms, _ := cell(t, cols, row, "milestone").AsText(); ms != "v1.0" {
			t.Errorf("milestone = %q, want v1.0", ms)
		}
		if got := textList(t, cell(t, cols, row, "labels")); !reflect.DeepEqual(got, []string{"bug", "question"}) {
			t.Errorf("labels = %v", got)
		}
		if got := textList(t, cell(t, cols, row, "assignees")); !reflect.DeepEqual(got, []string{"hubot"}) {
			t.Errorf("assignees = %v", got)
		}
		if state, _ := cell(t, cols, row, "state").AsText(); state != "open" {
			t.Errorf("state = %q", state)
		}
	})
	t.Run("issues with labels as comma text", func(t *testing.T) {
		key := insert(t, "issues", driver.Object{
			{Key: "title", Value: driver.String("Squeaky widget")},
			{Key: "labels", Value: driver.String("bug, wontfix")},
		})
		if sent := sentBody(t, lastWrite(t, f)); !reflect.DeepEqual(sent["labels"], []any{"bug", "wontfix"}) {
			t.Errorf("body sent = %v", sent)
		}
		cols, row := getRow(t, c, "issues", key)
		if got := textList(t, cell(t, cols, row, "labels")); !reflect.DeepEqual(got, []string{"bug", "wontfix"}) {
			t.Errorf("labels = %v", got)
		}
	})
	t.Run("pull requests", func(t *testing.T) {
		key := insert(t, "pull_requests", driver.Object{
			{Key: "title", Value: driver.String("Oil the widget")},
			{Key: "head", Value: driver.String("feature/oil")},
			{Key: "base", Value: driver.String("main")},
			{Key: "draft", Value: driver.Bool(true)},
			{Key: "body", Value: driver.String("Less squeak.")},
		})
		w := lastWrite(t, f)
		if w.Method != "POST" || w.Path != "/repos/octo/widgets/pulls" {
			t.Errorf("write = %+v", w)
		}
		if sent := sentBody(t, w); sent["head"] != "feature/oil" || sent["base"] != "main" || sent["draft"] != true {
			t.Errorf("body sent = %v", sent)
		}
		cols, row := getRow(t, c, "pull_requests", key)
		head, _ := cell(t, cols, row, "head").AsText()
		base, _ := cell(t, cols, row, "base").AsText()
		draft, _ := cell(t, cols, row, "draft").AsBool()
		if head != "feature/oil" || base != "main" || !draft {
			t.Errorf("read back head=%s base=%s draft=%v", head, base, draft)
		}
	})
	t.Run("issue comments with the issue as a number and as text", func(t *testing.T) {
		for _, issue := range []driver.Value{driver.Int(12), driver.String("12")} {
			key := insert(t, "issue_comments", driver.Object{
				{Key: "issue", Value: issue},
				{Key: "body", Value: driver.String("Looks fine.")},
			})
			w := lastWrite(t, f)
			if w.Path != "/repos/octo/widgets/issues/12/comments" {
				t.Errorf("issue %v: POST path = %s", issue, w.Path)
			}
			if sent := sentBody(t, w); sent["body"] != "Looks fine." || len(sent) != 1 {
				t.Errorf("body sent = %v, want the body alone", sent)
			}
			cols, row := getRow(t, c, "issue_comments", key)
			n, _ := cell(t, cols, row, "issue").AsInt()
			body, _ := cell(t, cols, row, "body").AsText()
			if n != 12 || body != "Looks fine." {
				t.Errorf("read back issue=%d body=%q", n, body)
			}
		}
	})
	t.Run("labels", func(t *testing.T) {
		key := insert(t, "labels", driver.Object{
			{Key: "name", Value: driver.String("needs-triage")},
			{Key: "color", Value: driver.String("ff8800")},
			{Key: "description", Value: driver.String("Not yet looked at")},
		})
		if name, _ := key.AsText(); name != "needs-triage" {
			t.Errorf("key = %v, want the name", key)
		}
		cols, row := getRow(t, c, "labels", key)
		color, _ := cell(t, cols, row, "color").AsText()
		desc, _ := cell(t, cols, row, "description").AsText()
		if color != "ff8800" || desc != "Not yet looked at" {
			t.Errorf("read back color=%s description=%q", color, desc)
		}
	})
	t.Run("milestones with due_on as a time and as text", func(t *testing.T) {
		due := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
		for i, v := range []driver.Value{driver.Time(due.In(time.FixedZone("x", 3600))), driver.String("2025-06-01T00:00:00Z")} {
			key := insert(t, "milestones", driver.Object{
				{Key: "title", Value: driver.String(fmt.Sprintf("v3.%d", i))},
				{Key: "description", Value: driver.String("Someday")},
				{Key: "due_on", Value: v},
			})
			if sent := sentBody(t, lastWrite(t, f)); sent["due_on"] != "2025-06-01T00:00:00Z" {
				t.Errorf("due_on sent = %v", sent["due_on"])
			}
			cols, row := getRow(t, c, "milestones", key)
			got, _ := cell(t, cols, row, "due_on").AsTime()
			state, _ := cell(t, cols, row, "state").AsText()
			if !got.Equal(due) || state != "open" {
				t.Errorf("read back due_on=%s state=%s", got, state)
			}
		}
	})
	t.Run("releases", func(t *testing.T) {
		key := insert(t, "releases", driver.Object{
			{Key: "tag_name", Value: driver.String("v3.0.0-beta")},
			{Key: "name", Value: driver.String("Widgets 3 beta")},
			{Key: "draft", Value: driver.Bool(true)},
			{Key: "prerelease", Value: driver.String("true")},
			{Key: "body", Value: driver.String("Try it.")},
		})
		if sent := sentBody(t, lastWrite(t, f)); sent["draft"] != true || sent["prerelease"] != true {
			t.Errorf("body sent = %v", sent)
		}
		cols, row := getRow(t, c, "releases", key)
		tag, _ := cell(t, cols, row, "tag_name").AsText()
		draft, _ := cell(t, cols, row, "draft").AsBool()
		pre, _ := cell(t, cols, row, "prerelease").AsBool()
		if tag != "v3.0.0-beta" || !draft || !pre || !cell(t, cols, row, "published_at").IsNull() {
			t.Errorf("read back tag=%s draft=%v prerelease=%v", tag, draft, pre)
		}
	})
}

// The comment's issue goes into the request path, so only a positive whole
// number is accepted; anything else is refused before a request.
func TestInsertCommentRefusesBadIssueNumbers(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.resetLog()
	for _, issue := range []driver.Value{driver.Int(0), driver.Int(-1), driver.String("1/../x"), driver.String("twelve"), driver.String(""), driver.Float(1.5), driver.Bool(true)} {
		_, err := c.Insert(context.Background(), ref("issue_comments"), driver.Object{{Key: "issue", Value: issue}, {Key: "body", Value: driver.String("x")}})
		wantKind(t, fmt.Sprintf("issue %v", issue), err, driver.ErrSyntax)
		if !strings.Contains(err.Error(), "issue number") {
			t.Errorf("issue %v: %v", issue, err)
		}
	}
	if n := len(f.log()); n != 0 {
		t.Errorf("refused inserts sent %d requests", n)
	}
}

// An insert missing a required member, naming the key, a computed column
// or an unknown one is refused before a request, with the message naming
// what is wrong.
func TestInsertRefusesBadMembers(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.resetLog()
	tests := []struct {
		name   string
		col    string
		values driver.Object
		kind   error
		hint   string
	}{
		{"issue without title", "issues", driver.Object{{Key: "body", Value: driver.String("x")}}, driver.ErrSyntax, `a new issue needs "title"`},
		{"issue with a null title", "issues", driver.Object{{Key: "title", Value: driver.Null()}}, driver.ErrSyntax, `needs "title"`},
		{"pull request without head", "pull_requests", driver.Object{{Key: "title", Value: driver.String("x")}, {Key: "base", Value: driver.String("main")}}, driver.ErrSyntax, `a new pull request needs "head"`},
		{"pull request without base", "pull_requests", driver.Object{{Key: "title", Value: driver.String("x")}, {Key: "head", Value: driver.String("f")}}, driver.ErrSyntax, `needs "base"`},
		{"comment without body", "issue_comments", driver.Object{{Key: "issue", Value: driver.Int(1)}}, driver.ErrSyntax, `needs "body"`},
		{"comment without issue", "issue_comments", driver.Object{{Key: "body", Value: driver.String("x")}}, driver.ErrSyntax, `needs "issue"`},
		{"label without name", "labels", driver.Object{{Key: "color", Value: driver.String("000000")}}, driver.ErrSyntax, `a new label needs "name"`},
		{"milestone without title", "milestones", driver.Object{{Key: "description", Value: driver.String("x")}}, driver.ErrSyntax, `a new milestone needs "title"`},
		{"release without tag", "releases", driver.Object{{Key: "name", Value: driver.String("x")}}, driver.ErrSyntax, `a new release needs "tag_name"`},
		{"issue number given", "issues", driver.Object{{Key: "number", Value: driver.Int(7)}, {Key: "title", Value: driver.String("x")}}, driver.ErrSyntax, "GitHub assigns the number of a new issue: leave it out"},
		{"release id given", "releases", driver.Object{{Key: "id", Value: driver.Int(7)}, {Key: "tag_name", Value: driver.String("x")}}, driver.ErrSyntax, "GitHub assigns the id of a new release"},
		{"comment id given", "issue_comments", driver.Object{{Key: "id", Value: driver.Int(7)}, {Key: "issue", Value: driver.Int(1)}, {Key: "body", Value: driver.String("x")}}, driver.ErrSyntax, "GitHub assigns the id"},
		{"computed column of an issue", "issues", driver.Object{{Key: "title", Value: driver.String("x")}, {Key: "comments", Value: driver.Int(3)}}, driver.ErrUnsupported, `"comments" cannot be set when creating issues (it can be set with: title, body, labels, assignees, milestone)`},
		{"computed column of a pull request", "pull_requests", driver.Object{{Key: "title", Value: driver.String("x")}, {Key: "merged_at", Value: driver.Time(time.Now())}}, driver.ErrUnsupported, "it can be set with: title, head, base, body, draft"},
		{"computed column of a release", "releases", driver.Object{{Key: "tag_name", Value: driver.String("x")}, {Key: "assets", Value: driver.Int(1)}}, driver.ErrUnsupported, "it can be set with: tag_name, target_commitish, name, body, draft, prerelease"},
		{"unknown column", "issues", driver.Object{{Key: "title", Value: driver.String("x")}, {Key: "priority", Value: driver.String("high")}}, driver.ErrSyntax, `issues have no column "priority"`},
		{"labels that are not names", "issues", driver.Object{{Key: "title", Value: driver.String("x")}, {Key: "labels", Value: driver.List(driver.Int(1))}}, driver.ErrSyntax, `"labels": expected a list of strings`},
		{"draft that is not a boolean", "pull_requests", driver.Object{{Key: "title", Value: driver.String("x")}, {Key: "head", Value: driver.String("f")}, {Key: "base", Value: driver.String("main")}, {Key: "draft", Value: driver.String("maybe")}}, driver.ErrSyntax, `"draft" must be true or false`},
		{"collection without inserts", "tags", driver.Object{{Key: "name", Value: driver.String("v9")}}, driver.ErrUnsupported, "does not create tags"},
		{"missing collection", "wiki", driver.Object{{Key: "title", Value: driver.String("x")}}, driver.ErrNotFound, `collection "wiki" does not exist`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Insert(context.Background(), ref(tc.col), tc.values)
			wantKind(t, tc.name, err, tc.kind)
			if !strings.Contains(err.Error(), tc.hint) {
				t.Errorf("%v does not mention %q", err, tc.hint)
			}
		})
	}
	if n := len(f.log()); n != 0 {
		t.Errorf("refused inserts sent %d requests: %+v", n, f.log())
	}
}

// Update changes each editable member of each collection, and the fake
// holds the new values; a NULL clears a member GitHub lets go empty.
func TestUpdateEachEditableMember(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	ctx := context.Background()
	update := func(t *testing.T, col string, key driver.Value, changes driver.Object) map[string]any {
		t.Helper()
		f.resetLog()
		if err := c.Update(ctx, ref(col), driver.Object{{Key: collectionByName(col).keyCol, Value: key}}, changes); err != nil {
			t.Fatalf("Update %s: %v", col, err)
		}
		w := lastWrite(t, f)
		if w.Method != "PATCH" {
			t.Errorf("write = %+v, want a PATCH", w)
		}
		return sentBody(t, w)
	}
	t.Run("issues", func(t *testing.T) {
		sent := update(t, "issues", driver.Int(7), driver.Object{
			{Key: "title", Value: driver.String("Renamed")},
			{Key: "body", Value: driver.String("New body")},
			{Key: "state", Value: driver.String("closed")},
			{Key: "state_reason", Value: driver.String("not_planned")},
			{Key: "labels", Value: driver.List(driver.String("wontfix"))},
			{Key: "assignees", Value: driver.String("octo,hubot")},
			{Key: "milestone", Value: driver.String("v1.0")},
		})
		if sent["milestone"] != 1.0 || sent["state_reason"] != "not_planned" || !reflect.DeepEqual(sent["assignees"], []any{"octo", "hubot"}) {
			t.Errorf("body sent = %v", sent)
		}
		if reqs := f.requestsTo("/repos/octo/widgets/milestones"); len(reqs) != 1 || reqs[0].Query.Get("state") != "all" {
			t.Errorf("the milestone title was resolved with %+v, want one listing of every milestone", reqs)
		}
		is := f.find("octo", "widgets", "issues", func(o object) bool { return num(o, "number") == 7 })
		if str(is, "title") != "Renamed" || str(is, "body") != "New body" || str(is, "state") != "closed" || str(is, "state_reason") != "not_planned" ||
			is["closed_at"] == nil || str(sub(is, "milestone"), "title") != "v1.0" ||
			!reflect.DeepEqual(memberNames(is, "labels", "name"), []string{"wontfix"}) || !reflect.DeepEqual(memberNames(is, "assignees", "login"), []string{"octo", "hubot"}) {
			t.Errorf("issue after update = %v", is)
		}
		sent = update(t, "issues", driver.Int(7), driver.Object{{Key: "milestone", Value: driver.Null()}, {Key: "body", Value: driver.Null()}, {Key: "state", Value: driver.String("open")}})
		if v, ok := sent["milestone"]; !ok || v != nil {
			t.Errorf("a NULL milestone was sent as %v, want null", sent)
		}
		if v, ok := sent["body"]; !ok || v != nil {
			t.Errorf("a NULL body was sent as %v, want null", sent)
		}
		is = f.find("octo", "widgets", "issues", func(o object) bool { return num(o, "number") == 7 })
		if is["milestone"] != nil || is["body"] != nil || str(is, "state") != "open" || is["closed_at"] != nil {
			t.Errorf("issue after clearing = %v", is)
		}
		cols, row := getRow(t, c, "issues", driver.Int(7))
		if !cell(t, cols, row, "milestone").IsNull() || !cell(t, cols, row, "body").IsNull() {
			t.Error("the cleared members read back with values")
		}
	})
	t.Run("pull requests", func(t *testing.T) {
		update(t, "pull_requests", driver.Int(251), driver.Object{
			{Key: "title", Value: driver.String("Retitled")},
			{Key: "body", Value: driver.String("Rewritten")},
			{Key: "state", Value: driver.String("closed")},
			{Key: "base", Value: driver.String("develop")},
		})
		pr := f.find("octo", "widgets", "pull_requests", func(o object) bool { return num(o, "number") == 251 })
		if str(pr, "title") != "Retitled" || str(pr, "body") != "Rewritten" || str(pr, "state") != "closed" || str(sub(pr, "base"), "ref") != "develop" {
			t.Errorf("pull request after update = %v", pr)
		}
	})
	t.Run("issue comments", func(t *testing.T) {
		cm := f.find("octo", "widgets", "issue_comments", func(object) bool { return true })
		update(t, "issue_comments", driver.Int(num(cm, "id")), driver.Object{{Key: "body", Value: driver.String("Edited")}})
		after := f.find("octo", "widgets", "issue_comments", func(o object) bool { return num(o, "id") == num(cm, "id") })
		if str(after, "body") != "Edited" || str(after, "updated_at") != editedAt {
			t.Errorf("comment after update = %v", after)
		}
	})
	t.Run("labels", func(t *testing.T) {
		sent := update(t, "labels", driver.String("question"), driver.Object{
			{Key: "color", Value: driver.String("123456")},
			{Key: "description", Value: driver.String("Ask away")},
		})
		if _, renamed := sent["new_name"]; renamed {
			t.Errorf("a label edit without a rename sent new_name: %v", sent)
		}
		l := f.find("octo", "widgets", "labels", func(o object) bool { return str(o, "name") == "question" })
		if str(l, "color") != "123456" || str(l, "description") != "Ask away" {
			t.Errorf("label after update = %v", l)
		}
	})
	t.Run("milestones", func(t *testing.T) {
		due := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		sent := update(t, "milestones", driver.Int(2), driver.Object{
			{Key: "title", Value: driver.String("v2.1")},
			{Key: "state", Value: driver.String("closed")},
			{Key: "description", Value: driver.String("Slipped")},
			{Key: "due_on", Value: driver.Time(due)},
		})
		if sent["due_on"] != "2026-01-02T03:04:05Z" {
			t.Errorf("due_on sent = %v", sent["due_on"])
		}
		m := f.find("octo", "widgets", "milestones", func(o object) bool { return num(o, "number") == 2 })
		if str(m, "title") != "v2.1" || str(m, "state") != "closed" || str(m, "description") != "Slipped" || str(m, "due_on") != "2026-01-02T03:04:05Z" {
			t.Errorf("milestone after update = %v", m)
		}
	})
	t.Run("releases", func(t *testing.T) {
		rel := f.find("octo", "widgets", "releases", func(o object) bool { return str(o, "tag_name") == "v1.1.0" })
		update(t, "releases", driver.Int(num(rel, "id")), driver.Object{
			{Key: "tag_name", Value: driver.String("v1.1.1")},
			{Key: "target_commitish", Value: driver.String("release/1.x")},
			{Key: "name", Value: driver.String("Widgets 1.1.1")},
			{Key: "body", Value: driver.String("Patch")},
			{Key: "draft", Value: driver.Bool(true)},
			{Key: "prerelease", Value: driver.Bool(true)},
		})
		after := f.find("octo", "widgets", "releases", func(o object) bool { return num(o, "id") == num(rel, "id") })
		draft, _ := after["draft"].(bool)
		pre, _ := after["prerelease"].(bool)
		if str(after, "tag_name") != "v1.1.1" || str(after, "target_commitish") != "release/1.x" || str(after, "name") != "Widgets 1.1.1" || str(after, "body") != "Patch" || !draft || !pre {
			t.Errorf("release after update = %v", after)
		}
	})
}

// Update refuses, before any request, a NULL on a required column, a key
// change, a computed column, an unknown column and a key naming the wrong
// member; empty changes do nothing; a missing item is ErrNotFound.
func TestUpdateRefusals(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	ctx := context.Background()
	f.resetLog()
	byNumber := driver.Object{{Key: "number", Value: driver.Int(1)}}
	tests := []struct {
		name    string
		col     string
		key     driver.Object
		changes driver.Object
		kind    error
		hint    string
	}{
		{"null title", "issues", byNumber, driver.Object{{Key: "title", Value: driver.Null()}}, driver.ErrSyntax, `"title" cannot be empty`},
		{"null label name", "labels", driver.Object{{Key: "name", Value: driver.String("bug")}}, driver.Object{{Key: "name", Value: driver.Null()}}, driver.ErrSyntax, `"name" cannot be empty`},
		{"null comment body", "issue_comments", driver.Object{{Key: "id", Value: driver.Int(1)}}, driver.Object{{Key: "body", Value: driver.Null()}}, driver.ErrSyntax, `"body" cannot be empty`},
		{"issue number", "issues", byNumber, driver.Object{{Key: "number", Value: driver.Int(2)}}, driver.ErrUnsupported, "the number of a issue cannot change"},
		{"milestone number", "milestones", byNumber, driver.Object{{Key: "number", Value: driver.Int(2)}}, driver.ErrUnsupported, "cannot change"},
		{"computed issue column", "issues", byNumber, driver.Object{{Key: "comments", Value: driver.Int(0)}}, driver.ErrUnsupported, `"comments" is computed by GitHub and cannot be edited (editable: title, body, state, state_reason, labels, assignees, milestone)`},
		{"computed pull request column", "pull_requests", byNumber, driver.Object{{Key: "head", Value: driver.String("x")}}, driver.ErrUnsupported, "editable: title, body, state, base"},
		{"unknown column", "issues", byNumber, driver.Object{{Key: "priority", Value: driver.String("high")}}, driver.ErrSyntax, `issues have no column "priority"`},
		{"wrong key member", "issues", driver.Object{{Key: "id", Value: driver.Int(1)}}, driver.Object{{Key: "title", Value: driver.String("x")}}, driver.ErrSyntax, `issues are keyed by "number", not "id"`},
		{"key without the key column", "issues", driver.Object{{Key: "id", Value: driver.Int(1)}, {Key: "title", Value: driver.String("x")}}, driver.Object{{Key: "title", Value: driver.String("x")}}, driver.ErrSyntax, `the key must give "number"`},
		{"empty key value", "labels", driver.Object{{Key: "name", Value: driver.String("")}}, driver.Object{{Key: "color", Value: driver.String("000000")}}, driver.ErrSyntax, `"name" must be a string or a number`},
		{"bad boolean", "releases", driver.Object{{Key: "id", Value: driver.Int(1)}}, driver.Object{{Key: "draft", Value: driver.String("maybe")}}, driver.ErrSyntax, `"draft" must be true or false`},
		{"collection without updates", "commits", driver.Object{{Key: "sha", Value: driver.String("abc")}}, driver.Object{{Key: "message", Value: driver.String("x")}}, driver.ErrUnsupported, "does not change commits"},
		{"missing collection", "wiki", byNumber, driver.Object{{Key: "title", Value: driver.String("x")}}, driver.ErrNotFound, `collection "wiki" does not exist`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := c.Update(ctx, ref(tc.col), tc.key, tc.changes)
			wantKind(t, tc.name, err, tc.kind)
			if !strings.Contains(err.Error(), tc.hint) {
				t.Errorf("%v does not mention %q", err, tc.hint)
			}
		})
	}
	if err := c.Update(ctx, ref("issues"), byNumber, nil); err != nil {
		t.Errorf("empty changes: %v", err)
	}
	if n := len(f.log()); n != 0 {
		t.Errorf("refused updates sent %d requests: %+v", n, f.log())
	}
	err := c.Update(ctx, ref("issues"), driver.Object{{Key: "number", Value: driver.Int(9999)}}, driver.Object{{Key: "title", Value: driver.String("x")}})
	wantKind(t, "a missing issue", err, driver.ErrNotFound)
	err = c.Update(ctx, ref("milestones"), driver.Object{{Key: "number", Value: driver.Int(99)}}, driver.Object{{Key: "title", Value: driver.String("x")}})
	wantKind(t, "a missing milestone", err, driver.ErrNotFound)
	err = c.Update(ctx, ref("issues"), byNumber, driver.Object{{Key: "milestone", Value: driver.String("v9.9")}})
	wantKind(t, "an unknown milestone title", err, driver.ErrNotFound)
	if !strings.Contains(err.Error(), `no milestone titled "v9.9"`) {
		t.Errorf("milestone refusal = %v", err)
	}
	if w := f.writes(); len(w) != 2 {
		t.Errorf("writes = %+v, want the two PATCHes GitHub answered 404", w)
	}
}

// Delete removes an item of each collection GitHub deletes through tobira;
// the fake no longer has it.
func TestDeleteEachDeletableCollection(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	comment := f.find("octo", "widgets", "issue_comments", func(object) bool { return true })
	release := f.find("octo", "widgets", "releases", func(o object) bool { return str(o, "tag_name") == "v1.0.0" })
	tests := []struct {
		col   string
		key   driver.Value
		path  string
		match func(object) bool
	}{
		{"issue_comments", driver.Int(num(comment, "id")), "/repos/octo/widgets/issues/comments/" + itoa(num(comment, "id")), func(o object) bool { return num(o, "id") == num(comment, "id") }},
		{"labels", driver.String("good first issue"), "/repos/octo/widgets/labels/good%20first%20issue", func(o object) bool { return str(o, "name") == "good first issue" }},
		{"milestones", driver.Int(3), "/repos/octo/widgets/milestones/3", func(o object) bool { return num(o, "number") == 3 }},
		{"releases", driver.Int(num(release, "id")), "/repos/octo/widgets/releases/" + itoa(num(release, "id")), func(o object) bool { return num(o, "id") == num(release, "id") }},
	}
	for _, tc := range tests {
		t.Run(tc.col, func(t *testing.T) {
			if f.find("octo", "widgets", tc.col, tc.match) == nil {
				t.Fatal("the item to delete is not seeded")
			}
			before := f.count("octo", "widgets", tc.col)
			f.resetLog()
			if err := c.Delete(context.Background(), ref(tc.col), driver.Object{{Key: collectionByName(tc.col).keyCol, Value: tc.key}}); err != nil {
				t.Fatal(err)
			}
			w := lastWrite(t, f)
			if w.Method != "DELETE" || w.Status != 204 || len(w.Body) != 0 {
				t.Errorf("write = %+v", w)
			}
			if got := w.Path; got != tc.path && got != strings.ReplaceAll(tc.path, "%20", " ") {
				t.Errorf("DELETE path = %s, want %s", got, tc.path)
			}
			if f.find("octo", "widgets", tc.col, tc.match) != nil || f.count("octo", "widgets", tc.col) != before-1 {
				t.Error("the item is still there")
			}
			err := c.Delete(context.Background(), ref(tc.col), driver.Object{{Key: collectionByName(tc.col).keyCol, Value: tc.key}})
			wantKind(t, "a second delete", err, driver.ErrNotFound)
		})
	}
}

// Issues and pull requests cannot be deleted through the REST API (the
// error says to close them); collections without writes are
// ErrUnsupported. Nothing is sent.
func TestDeleteRefusals(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.resetLog()
	ctx := context.Background()
	for col, hint := range map[string]string{"issues": "cannot delete issue", "pull_requests": "cannot delete pull request"} {
		err := c.Delete(ctx, ref(col), driver.Object{{Key: "number", Value: driver.Int(1)}})
		wantKind(t, "Delete of "+col, err, driver.ErrUnsupported)
		if !strings.Contains(err.Error(), hint) || !strings.Contains(err.Error(), "close it instead (set state to closed)") {
			t.Errorf("%s: %v", col, err)
		}
	}
	for col, key := range map[string]driver.Object{
		"commits":       {{Key: "sha", Value: driver.String("abc")}},
		"branches":      {{Key: "name", Value: driver.String("main")}},
		"tags":          {{Key: "name", Value: driver.String("v1.0.0")}},
		"workflow_runs": {{Key: "id", Value: driver.Int(9001)}},
	} {
		err := c.Delete(ctx, ref(col), key)
		wantKind(t, "Delete of "+col, err, driver.ErrUnsupported)
		if !strings.Contains(err.Error(), "does not delete "+col) {
			t.Errorf("%s: %v", col, err)
		}
	}
	err := c.Delete(ctx, ref("labels"), driver.Object{{Key: "id", Value: driver.Int(1)}})
	wantKind(t, "a label keyed by id", err, driver.ErrSyntax)
	err = c.Delete(ctx, ref("wiki"), driver.Object{{Key: "id", Value: driver.Int(1)}})
	wantKind(t, "a missing collection", err, driver.ErrNotFound)
	if n := len(f.log()); n != 0 {
		t.Errorf("refused deletes sent %d requests: %+v", n, f.log())
	}
	if f.count("octo", "widgets", "issues") != 270 || f.count("octo", "widgets", "branches") != 5 {
		t.Error("something was deleted")
	}
}

// A 422 from GitHub is ErrSyntax carrying the field and code of the
// validation error, with the status as Error.Code.
func TestWriteValidationFailureNamesTheField(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	ctx := context.Background()
	_, err := c.Insert(ctx, ref("labels"), driver.Object{{Key: "name", Value: driver.String("bug")}})
	wantKind(t, "a duplicate label", err, driver.ErrSyntax)
	if msg := err.Error(); !strings.Contains(msg, "Validation Failed") || !strings.Contains(msg, "name") || !strings.Contains(msg, "already_exists") {
		t.Errorf("the error lacks the field detail: %v", err)
	}
	if de := asDriverError(t, err); de.Code != "422" || de.Op != "insert" {
		t.Errorf("code = %q op = %q, want 422 insert", de.Code, de.Op)
	}
	_, err = c.Insert(ctx, ref("issues"), driver.Object{{Key: "title", Value: driver.String("x")}, {Key: "milestone", Value: driver.Int(99)}})
	wantKind(t, "an unknown milestone number", err, driver.ErrSyntax)
	if msg := err.Error(); !strings.Contains(msg, "milestone") || !strings.Contains(msg, "invalid") {
		t.Errorf("the error lacks the field detail: %v", err)
	}
	err = c.Update(ctx, ref("issues"), driver.Object{{Key: "number", Value: driver.Int(1)}}, driver.Object{{Key: "milestone", Value: driver.Int(99)}})
	wantKind(t, "an update with an unknown milestone number", err, driver.ErrSyntax)
	if de := asDriverError(t, err); de.Code != "422" || de.Op != "update" {
		t.Errorf("code = %q op = %q, want 422 update", de.Code, de.Op)
	}
	if f.count("octo", "widgets", "labels") != 7 || f.count("octo", "widgets", "issues") != 270 {
		t.Error("a refused write changed the data")
	}
}

// A write drops the repository names kept for completion: the next
// completion lists them from the server again.
func TestWritesInvalidateTheCompletionNames(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.openDSN(t, f.dsn("octo", ""), false, fakeToken)
	ctx := context.Background()
	repos := func(t *testing.T) []string {
		t.Helper()
		cs, err := c.Complete(ctx, driver.CompletionRequest{Text: `{"repo":"`, Cursor: 9})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, comp := range cs {
			out = append(out, comp.Label)
		}
		return out
	}
	listings := func() int { return len(f.requestsTo("/user/repos")) }
	cached := func() bool {
		c.catalog.mu.Lock()
		defer c.catalog.mu.Unlock()
		return c.catalog.names != nil
	}
	if got := repos(t); !reflect.DeepEqual(got, []string{"octo", "octo/gadgets", "octo/widgets"}) {
		t.Fatalf("repositories = %v", got)
	}
	if listings() != 1 || !cached() {
		t.Fatalf("after one completion: %d listings, cached %v", listings(), cached())
	}
	writes := map[string]func() error{
		"insert": func() error {
			_, err := c.Insert(ctx, ref("labels"), driver.Object{{Key: "name", Value: driver.String("fresh")}})
			return err
		},
		"update": func() error {
			return c.Update(ctx, ref("labels"), driver.Object{{Key: "name", Value: driver.String("fresh")}}, driver.Object{{Key: "color", Value: driver.String("000000")}})
		},
		"delete": func() error {
			return c.Delete(ctx, ref("labels"), driver.Object{{Key: "name", Value: driver.String("fresh")}})
		},
	}
	for i, name := range []string{"insert", "update", "delete"} {
		if err := writes[name](); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if cached() {
			t.Errorf("after %s the names are still cached", name)
		}
		repos(t)
		if got, want := listings(), i+2; got != want {
			t.Errorf("after %s: %d listings, want %d", name, got, want)
		}
	}
	repos(t)
	if listings() != 4 {
		t.Errorf("a completion with fresh names sent a listing again: %d", listings())
	}
}

// On a read-only connection the three writes are refused for every
// collection, the read-only refusal coming before any other (a collection
// without writes is ErrReadOnly too), and nothing is sent.
func TestReadOnlyRefusesWritesToEveryCollection(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, true)
	f.resetLog()
	ctx := context.Background()
	for _, col := range collections {
		key := driver.Object{{Key: col.keyCol, Value: driver.String("1")}}
		_, err := c.Insert(ctx, ref(col.name), driver.Object{{Key: "title", Value: driver.String("x")}})
		wantKind(t, "Insert into "+col.name, err, driver.ErrReadOnly)
		err = c.Update(ctx, ref(col.name), key, driver.Object{{Key: "title", Value: driver.String("x")}})
		wantKind(t, "Update of "+col.name, err, driver.ErrReadOnly)
		err = c.Delete(ctx, ref(col.name), key)
		wantKind(t, "Delete of "+col.name, err, driver.ErrReadOnly)
		if !strings.Contains(err.Error(), "read-only") {
			t.Errorf("%s: %v", col.name, err)
		}
	}
	if n := len(f.log()); n != 0 {
		t.Errorf("read-only writes sent %d requests", n)
	}
}
