package github

import (
	"context"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// writeQueries are the query texts a read-only guard must refuse: plain
// writes, writes hidden in a GraphQL document, and texts the guard cannot
// read with confidence.
var writeQueries = map[string]string{
	"rest POST":                   `{"op":"rest","method":"POST","path":"/repos/octo/widgets/labels","body":{"name":"x"}}`,
	"rest PATCH":                  `{"op":"rest","method":"patch","path":"/repos/octo/widgets/labels/bug","body":{"color":"000000"}}`,
	"rest DELETE":                 `{"op":"rest","method":"DELETE","path":"/repos/octo/widgets/labels/bug"}`,
	"rest PUT":                    `{"op":"rest","method":"PUT","path":"/repos/octo/widgets/topics","body":{"names":[]}}`,
	"graphql mutation":            `{"op":"graphql","query":"mutation { addComment(input:{subjectId:\"x\",body:\"y\"}) { clientMutationId } }"}`,
	"mutation after a comment":    `{"op":"graphql","query":"# just a read\nmutation { deleteIssue(input:{issueId:\"x\"}) { clientMutationId } }"}`,
	"mutation inside braces":      `{"op":"graphql","query":"query Q { viewer { login } } mutation M { deleteIssue(input:{issueId:\"x\"}) { clientMutationId } }"}`,
	"mutation with a comment":     `{"op":"graphql","query":"query { viewer { login } }\n# nothing\nmutation { x }"}`,
	"document starting fragment":  `{"op":"graphql","query":"fragment F on User { login } query { viewer { ...F } }"}`,
	"subscription":                `{"op":"graphql","query":"subscription { x }"}`,
	"duplicate op keys":           `{"op":"list","collection":"issues","op":"rest","method":"POST","path":"/repos/octo/widgets/labels"}`,
	"two JSON objects":            `{"collection":"issues"} {"op":"rest","method":"POST","path":"/repos/octo/widgets/labels"}`,
	"unknown op":                  `{"op":"write","path":"/repos/octo/widgets/labels"}`,
	"malformed":                   `{"op":"rest",`,
	"unknown member":              `{"op":"rest","path":"/repos/octo/widgets/labels","mode":"POST"}`,
	"method that is not a string": `{"op":"rest","method":["POST"],"path":"/repos/octo/widgets/labels"}`,
}

// notJSONQueries are texts that are not a JSON object at all: nothing could
// run, so they are ErrSyntax with the hint on the JSON shape, read-only or
// not.
var notJSONQueries = map[string]string{
	"array":      `["op","rest"]`,
	"string":     `"list"`,
	"SQL":        `select 1`,
	"empty text": ``,
	"blank text": `   `,
}

// On a read-only connection every write, compound form or unreadable text
// is refused with ErrReadOnly before any request, a text that is not JSON
// is ErrSyntax with the hint on the shape, and reads still run.
func TestReadOnlyConnectionRefusesWrites(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, true)
	f.resetLog()
	for name, text := range writeQueries {
		_, err := c.Query(context.Background(), driver.Query{Text: text})
		if err == nil || !isKind(err, driver.ErrReadOnly) {
			t.Errorf("%s: %v, want ErrReadOnly", name, err)
		}
	}
	for name, text := range notJSONQueries {
		_, err := c.Query(context.Background(), driver.Query{Text: text})
		if err == nil || !isKind(err, driver.ErrSyntax) || !strings.Contains(err.Error(), "one JSON object") {
			t.Errorf("%s: %v, want ErrSyntax naming the JSON shape", name, err)
		}
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("the fake saw writes: %+v", w)
	}
	if n := len(f.log()); n != 0 {
		t.Errorf("a refused query sent %d requests", n)
	}
	for _, text := range []string{
		`{"collection":"labels"}`,
		`{"op":"get","collection":"issues","number":1}`,
		`{"op":"rest","path":"/repos/octo/widgets/labels"}`,
		`{"op":"rest","method":"get","path":"/repos/octo/widgets"}`,
		`{"op":"graphql","query":"{ viewer { login } }"}`,
		`{"op":"graphql","query":"query Who { viewer { login } }"}`,
		`{"op":"graphql","query":"# a mutation in a comment\nquery { viewer { login } }"}`,
		`{"op":"graphql","query":"{ search(query: \"mutation\", type: ISSUE, first: 1) { issueCount } }"}`,
		`{"op":"search","type":"issues","q":"repo:octo/widgets is:open"}`,
		`{"op":"rate_limit"}`,
		`{"op":"repositories"}`,
	} {
		_, rows, _ := queryText(t, c, text)
		if len(rows) == 0 {
			t.Errorf("%s: no rows on a read-only connection", text)
		}
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("a read sent a write: %+v", w)
	}
}

// Query.ReadOnly on a writable connection is the same guard.
func TestQueryReadOnlyRefusesWrites(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.resetLog()
	for name, text := range writeQueries {
		_, err := c.Query(context.Background(), driver.Query{Text: text, ReadOnly: true})
		if err == nil || !isKind(err, driver.ErrReadOnly) {
			t.Errorf("%s: %v, want ErrReadOnly", name, err)
		}
	}
	for name, text := range notJSONQueries {
		_, err := c.Query(context.Background(), driver.Query{Text: text, ReadOnly: true})
		if err == nil || !isKind(err, driver.ErrSyntax) {
			t.Errorf("%s: %v, want ErrSyntax", name, err)
		}
	}
	if n := len(f.log()); n != 0 {
		t.Errorf("a refused query sent %d requests: %+v", n, f.log())
	}
	if _, rows, _ := queryWith(t, c, driver.Query{Text: `{"collection":"labels"}`, ReadOnly: true}); len(rows) != 7 {
		t.Errorf("a read with Query.ReadOnly: %d rows, want 7", len(rows))
	}
}

// Explain under the guard refuses a write too, so a plan never runs one.
func TestExplainReadOnlyRefusesWrites(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	_, err := c.Explain(context.Background(), driver.Query{Text: writeQueries["rest POST"], ReadOnly: true}, driver.ExplainOptions{})
	wantKind(t, "Explain of a rest POST read-only", err, driver.ErrReadOnly)
	ro := f.open(t, true)
	_, err = ro.Explain(context.Background(), driver.Query{Text: writeQueries["graphql mutation"]}, driver.ExplainOptions{})
	wantKind(t, "Explain of a mutation on a read-only connection", err, driver.ErrReadOnly)
}

// The same texts run on a writable connection: the guard, not the parser,
// refused them. (The compound and malformed ones are ErrSyntax there.)
func TestWritableConnectionRunsOrRejectsThePlainWay(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	for name, kind := range map[string]error{
		"duplicate op keys": driver.ErrSyntax, "two JSON objects": driver.ErrSyntax, "unknown op": driver.ErrSyntax,
		"malformed": driver.ErrSyntax, "unknown member": driver.ErrSyntax, "method that is not a string": driver.ErrSyntax,
	} {
		_, err := c.Query(context.Background(), driver.Query{Text: writeQueries[name]})
		if !isKind(err, kind) {
			t.Errorf("%s on a writable connection: %v, want %v", name, err, kind)
		}
	}
	for name, text := range notJSONQueries {
		_, err := c.Query(context.Background(), driver.Query{Text: text})
		if !isKind(err, driver.ErrSyntax) {
			t.Errorf("%s on a writable connection: %v, want ErrSyntax", name, err)
		}
	}
	f.resetLog()
	if _, _, _ = queryText(t, c, writeQueries["rest PATCH"]); len(f.writes()) != 1 {
		t.Errorf("a rest PATCH on a writable connection sent %d writes", len(f.writes()))
	}
	queryText(t, c, writeQueries["graphql mutation"])
	if m := f.mutationLog(); len(m) != 1 {
		t.Errorf("a mutation on a writable connection reached the server %d times", len(m))
	}
}

// Insert, Update and Delete on a read-only connection fail before any
// request.
func TestReadOnlyMutatorRefusesBeforeSending(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, true)
	f.resetLog()
	ctx := context.Background()
	issues := driver.CollectionRef{Namespace: "octo/widgets", Name: "issues"}
	labels := driver.CollectionRef{Namespace: "octo/widgets", Name: "labels"}
	_, err := c.Insert(ctx, issues, driver.Object{{Key: "title", Value: driver.String("New")}})
	wantKind(t, "Insert read-only", err, driver.ErrReadOnly)
	err = c.Update(ctx, issues, driver.Object{{Key: "number", Value: driver.Int(1)}}, driver.Object{{Key: "title", Value: driver.String("New")}})
	wantKind(t, "Update read-only", err, driver.ErrReadOnly)
	err = c.Delete(ctx, labels, driver.Object{{Key: "name", Value: driver.String("bug")}})
	wantKind(t, "Delete read-only", err, driver.ErrReadOnly)
	if n := len(f.log()); n != 0 {
		t.Errorf("read-only writes sent %d requests: %+v", n, f.log())
	}
	if f.find("octo", "widgets", "labels", func(o object) bool { return str(o, "name") == "bug" }) == nil {
		t.Error("the label was deleted on a read-only connection")
	}
}

func isKind(err error, kind error) bool { return driver.KindOf(err) == kind }
