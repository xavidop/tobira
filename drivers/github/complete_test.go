package github

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// completeAt asks for completions with the cursor at the end of text.
func completeAt(t *testing.T, c *conn, text string) []driver.Completion {
	t.Helper()
	cs, err := c.Complete(context.Background(), driver.CompletionRequest{Text: text, Cursor: len(text)})
	if err != nil {
		t.Fatalf("complete %q: %v", text, err)
	}
	return cs
}

// labelsOf lists the labels of completions, checking they all share kind.
func labelsOf(t *testing.T, cs []driver.Completion, kind string) []string {
	t.Helper()
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		if c.Kind != kind {
			t.Errorf("%q has kind %q, want %q", c.Label, c.Kind, kind)
		}
		if c.Insert != "" && c.Insert != c.Label {
			t.Errorf("%q inserts %q", c.Label, c.Insert)
		}
		out = append(out, c.Label)
	}
	return out
}

// After "op":", "collection":", "type":", "method":", "sort":", "order":"
// or "state":" the values that member takes are offered, filtered by the
// typed prefix without regard to case.
func TestCompleteValuesOfKnownMembers(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	tests := []struct {
		text string
		kind string
		want []string
	}{
		{`{"op":"`, "keyword", []string{"get", "graphql", "list", "rate_limit", "repositories", "rest", "search"}},
		{`{"op": "`, "keyword", []string{"get", "graphql", "list", "rate_limit", "repositories", "rest", "search"}},
		{`{"op":"r`, "keyword", []string{"rate_limit", "repositories", "rest"}},
		{`{"op":"RE`, "keyword", []string{"repositories", "rest"}},
		{`{"op":"Graph`, "keyword", []string{"graphql"}},
		{`{"op":"zzz`, "keyword", []string{}},
		{`{"collection":"`, "collection", collectionNames()},
		{`{"op":"list","collection":"issue`, "collection", []string{"issues", "issue_comments"}},
		{`{"op":"get","collection":"P`, "collection", []string{"pull_requests"}},
		{`{"op":"search","type":"`, "keyword", []string{"issues", "pull_requests", "commits", "repositories"}},
		{`{"op":"search","type":"c`, "keyword", []string{"commits"}},
		{`{"op":"rest","method":"`, "keyword", []string{"GET", "POST", "PATCH", "PUT", "DELETE"}},
		{`{"op":"rest","method":"p`, "keyword", []string{"POST", "PATCH", "PUT"}},
		{`{"op":"list","collection":"issues","sort":"`, "field", []string{"comments", "created_at", "updated_at"}},
		{`{"collection":"milestones","sort":"`, "field", []string{"due_on"}},
		{`{"collection":"pull_requests","sort":"up`, "field", []string{"updated_at"}},
		{`{"collection":"tags","sort":"`, "field", []string{}},
		{`{"sort":"`, "field", []string{}},
		{`{"op":"search","type":"issues","sort":"`, "keyword", []string{"created", "updated", "comments", "reactions", "interactions", "stars", "forks", "author-date", "committer-date"}},
		{`{"op":"search","type":"commits","sort":"author`, "keyword", []string{"author-date"}},
		{`{"op":"search","order":"`, "keyword", []string{"asc", "desc"}},
		{`{"collection":"issues","direction":"d`, "keyword", []string{"desc"}},
		{`{"collection":"issues","state":"`, "keyword", []string{"open", "closed", "all"}},
		{`{"collection":"issues","state":"c`, "keyword", []string{"closed"}},
	}
	for _, tc := range tests {
		t.Run(tc.text, func(t *testing.T) {
			got := labelsOf(t, completeAt(t, c, tc.text), tc.kind)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("completions = %v, want %v", got, tc.want)
			}
		})
	}
}

// After "repo":" the names come from Namespaces, the owner first and then
// its repositories, kept for later completions; Invalidate makes the next
// one list them again.
func TestCompleteRepositoryNamesAreCached(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.openDSN(t, f.dsn("octo", ""), false, fakeToken)
	f.resetLog()
	listings := func() int { return len(f.requestsTo("/user/repos")) }
	got := labelsOf(t, completeAt(t, c, `{"collection":"issues","repo":"`), "namespace")
	if want := []string{"octo", "octo/gadgets", "octo/widgets"}; !reflect.DeepEqual(got, want) {
		t.Errorf("repositories = %v, want %v", got, want)
	}
	if listings() != 1 {
		t.Fatalf("%d listings after the first completion", listings())
	}
	if got := labelsOf(t, completeAt(t, c, `{"repo":"octo/w`), "namespace"); !reflect.DeepEqual(got, []string{"octo/widgets"}) {
		t.Errorf("prefixed repositories = %v", got)
	}
	if got := labelsOf(t, completeAt(t, c, `{"repo":"OCTO/G`), "namespace"); !reflect.DeepEqual(got, []string{"octo/gadgets"}) {
		t.Errorf("case-insensitive prefix = %v", got)
	}
	if listings() != 1 {
		t.Errorf("%d listings after three completions, want the names kept", listings())
	}
	c.Invalidate()
	if got := labelsOf(t, completeAt(t, c, `{"repo":"`), "namespace"); len(got) != 3 {
		t.Errorf("repositories after Invalidate = %v", got)
	}
	if listings() != 2 {
		t.Errorf("%d listings after Invalidate, want a fresh one", listings())
	}
	var inv driver.CatalogInvalidator = c
	inv.Invalidate()
	completeAt(t, c, `{"repo":"`)
	if listings() != 3 {
		t.Errorf("%d listings after the interface's Invalidate", listings())
	}

	// A pinned repository is the only name, and comes from the repository
	// cache, so no listing is sent.
	pinned := f.open(t, false)
	f.resetLog()
	if got := labelsOf(t, completeAt(t, pinned, `{"repo":"`), "namespace"); !reflect.DeepEqual(got, []string{"octo/widgets"}) {
		t.Errorf("pinned repositories = %v", got)
	}
	if n := len(f.log()); n != 0 {
		t.Errorf("completing the pinned repository sent %d requests", n)
	}
	// Without a token the listing fails and no names are offered.
	anonymous := f.openDSN(t, f.dsn("", ""), false, "")
	if got := completeAt(t, anonymous, `{"repo":"`); len(got) != 0 {
		t.Errorf("anonymous repositories = %v", got)
	}
	// An organization owner is offered first too, and a prefix that is the
	// owner's name keeps the owner and its repositories.
	org := f.openDSN(t, f.dsn("acme", ""), false, fakeToken)
	if got := labelsOf(t, completeAt(t, org, `{"repo":"`), "namespace"); !reflect.DeepEqual(got, []string{"acme", "acme/alpha", "acme/beta"}) {
		t.Errorf("organization names = %v", got)
	}
	if got := labelsOf(t, completeAt(t, org, `{"repo":"acme/`), "namespace"); !reflect.DeepEqual(got, []string{"acme/alpha", "acme/beta"}) {
		t.Errorf("names under acme/ = %v", got)
	}
}

// At a key position (after "{" or ",") the op, the op's members and, for
// a list, the collection's parameters are offered; for a get, the key
// members, full_name and repository (the keys of an owner's collections)
// among them.
func TestCompleteKeysAtAKeyPosition(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	listKeys := []string{"op", "collection", "repo", "limit"}
	getKeys := []string{"op", "collection", "repo", "key", "number", "id", "sha", "name", "full_name", "repository"}
	issueParams := collectionByName("issues").params
	tests := []struct {
		text string
		want []string
	}{
		{`{`, listKeys},
		{`{"`, listKeys},
		{`{ "`, listKeys},
		{``, listKeys},
		{`{"collection":"issues",`, append(append([]string{}, listKeys...), issueParams...)},
		{`{"op":"list","collection":"issues", "`, append(append([]string{}, listKeys...), issueParams...)},
		{`{"op":"list","collection":"issues","la`, []string{"labels"}},
		{`{"op":"list","collection":"issues","S`, []string{"state", "since", "sort"}},
		{`{"op":"list",`, listKeys},
		{`{"op":"get","collection":"issues",`, getKeys},
		{`{"op":"get","collection":"tags","n`, []string{"number", "name"}},
		{`{"op":"get",`, getKeys},
		{`{"op":"get","collection":"repositories","f`, []string{"full_name"}},
		{`{"op":"get","collection":"issues","rep`, []string{"repo", "repository"}},
		{`{"op":"search",`, []string{"op", "type", "q", "sort", "order", "limit"}},
		{`{"op":"rest",`, []string{"op", "method", "path", "params", "body", "limit"}},
		{`{"op":"graphql",`, []string{"op", "query", "variables"}},
		{`{"op":"rate_limit",`, []string{"op"}},
		{`{"op":"repositories","l`, []string{"limit"}},
		{`{"op":"fly",`, []string{"op"}},
	}
	for _, tc := range tests {
		t.Run(tc.text, func(t *testing.T) {
			got := labelsOf(t, completeAt(t, c, tc.text), "keyword")
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("completions = %v, want %v", got, tc.want)
			}
		})
	}
	// The key column of a get is offered as a field when the op's members
	// do not already name it; every key column is one of them, so it shows
	// once, as a keyword.
	got := completeAt(t, c, `{"op":"get","collection":"issue_comments",`)
	seen := 0
	for _, comp := range got {
		if comp.Label == "id" {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("the key column appears %d times in %v", seen, got)
	}
}

// A bare word elsewhere, with a known collection, completes to its
// columns; without one nothing is offered.
func TestCompleteColumnsForABareWord(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	tests := []struct {
		text string
		want []string
	}{
		{`{"collection":"issues","x":ti`, []string{"title"}},
		{`{"collection":"issues","q":"a`, []string{"assignees", "author_association"}},
		{`{"collection":"branches","x":`, []string{"name", "sha", "protected"}},
		{`{"collection":"issues","limit":1} cr`, []string{"created_at"}},
		{`{"collection":"issues","x":Upd`, []string{"updated_at"}},
		{`{"op":"search","q":"ti`, nil},
		{`{"collection":"wiki","x":ti`, nil},
		{`{"x":ti`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.text, func(t *testing.T) {
			got := labelsOf(t, completeAt(t, c, tc.text), "field")
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("completions = %v, want %v", got, tc.want)
			}
		})
	}
}

// The cursor is clamped to the text, the word at the cursor decides the
// prefix (text after it is ignored), answers never exceed the cap, and a
// closed connection refuses.
func TestCompleteCursorAndLimits(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	ctx := context.Background()
	text := `{"op":"re","collection":"issues"}`
	for cursor, want := range map[int][]string{
		7: {"get", "graphql", "list", "rate_limit", "repositories", "rest", "search"},
		8: {"rate_limit", "repositories", "rest"},
		9: {"repositories", "rest"},
	} {
		cs, err := c.Complete(ctx, driver.CompletionRequest{Text: text, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if got := labelsOf(t, cs, "keyword"); !reflect.DeepEqual(got, want) {
			t.Errorf("cursor %d: %v, want %v", cursor, got, want)
		}
	}
	// Past the closing quote the word is over: the collection named later
	// in the text is known, so its columns are offered.
	if got := labelsOf(t, func() []driver.Completion {
		cs, err := c.Complete(ctx, driver.CompletionRequest{Text: text, Cursor: 10})
		if err != nil {
			t.Fatal(err)
		}
		return cs
	}(), "field"); len(got) != len(issueColumns) {
		t.Errorf("cursor 10: %v, want the issue columns", got)
	}
	beyond, err := c.Complete(ctx, driver.CompletionRequest{Text: `{"op":"`, Cursor: 999})
	if err != nil {
		t.Fatal(err)
	}
	if got := labelsOf(t, beyond, "keyword"); len(got) != len(opKeys) {
		t.Errorf("a cursor past the text = %v, want every op", got)
	}
	negative, err := c.Complete(ctx, driver.CompletionRequest{Text: `{"op":"`, Cursor: -5})
	if err != nil {
		t.Fatal(err)
	}
	if got := labelsOf(t, negative, "keyword"); !reflect.DeepEqual(got, []string{"op", "collection", "repo", "limit"}) {
		t.Errorf("a negative cursor = %v, want the keys of an empty text", got)
	}
	empty, err := c.Complete(ctx, driver.CompletionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := labelsOf(t, empty, "keyword"); !reflect.DeepEqual(got, []string{"op", "collection", "repo", "limit"}) {
		t.Errorf("an empty request = %v", got)
	}
	for _, text := range []string{`{"collection":"issues",`, `{"collection":"workflow_runs","x":`, `{"op":"`, `{"repo":"`} {
		if n := len(completeAt(t, c, text)); n > maxCompletions || n == 0 {
			t.Errorf("%s: %d completions", text, n)
		}
	}
	// Mid-document positions inside a value that is not a known member
	// offer nothing rather than guessing.
	if got := completeAt(t, c, `{"collection":"issues","q":"x y`); len(got) != 0 {
		t.Errorf("inside a free-text value = %v", got)
	}
	// Every completion kind is one the TUI knows.
	kinds := map[string]bool{}
	for _, text := range []string{`{"op":"`, `{"collection":"`, `{"repo":"`, `{"collection":"issues","x":`} {
		for _, comp := range completeAt(t, c, text) {
			kinds[comp.Kind] = true
		}
	}
	if want := map[string]bool{"keyword": true, "collection": true, "namespace": true, "field": true}; !reflect.DeepEqual(kinds, want) {
		t.Errorf("kinds = %v, want %v", kinds, want)
	}

	closed := f.open(t, false)
	_ = closed.Close()
	_, err = closed.Complete(ctx, driver.CompletionRequest{Text: `{"op":"`, Cursor: 7})
	wantKind(t, "a closed connection", err, driver.ErrConnection)
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("closed refusal = %v", err)
	}
}
