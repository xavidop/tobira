package github

import (
	"context"
	"encoding/json"
	"testing"

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
