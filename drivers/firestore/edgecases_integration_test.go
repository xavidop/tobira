//go:build integration

package firestore

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// With FIRESTORE_EMULATOR_HOST equal to the DSN's emulator=, the client
// library dials the emulator itself and the connection works.
func TestEmulatorHostEnvAgreesLive(t *testing.T) {
	t.Setenv(emulatorEnv, emulatorAddr)
	c := openTest(t, testProject, false)
	if _, rows, _ := collect(t, c, driver.Query{Text: `{"document":"users/ada"}`}); len(rows) != 1 {
		t.Errorf("rows = %d", len(rows))
	}
	if _, err := New().Open(ctxT(t), driver.ConnConfig{DSN: "firestore://" + testProject}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("DSN without emulator= while the variable is set: %v", err)
	}
}

// A stored _id field shows as _id_field, with a warning, and can be
// filtered and edited; backquotes reach a field whose name holds dots.
func TestStoredIDAndDottedNames(t *testing.T) {
	a := admin(ctxT(t), t, testProject)
	coll := a.Collection("tobira_odd")
	if _, err := coll.Doc("x").Set(ctxT(t), map[string]any{"_id": "stored", "a.b": int64(7), "a": map[string]any{"b": int64(8)}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = coll.Doc("x").Delete(context.Background()) })
	c := openTest(t, testProject, false)
	cols, rows, st := collect(t, c, driver.Query{Text: `{"collection":"tobira_odd"}`})
	i := col(t, cols, "_id_field")
	if len(rows) != 1 || rows[0][i].String() != "stored" || rows[0][col(t, cols, "_id")].String() != "x" {
		t.Fatalf("rows = %v cols %+v", rows, cols)
	}
	if len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], "_id_field") {
		t.Errorf("warnings = %v", st.Warnings)
	}
	for text, want := range map[string]int{
		`{"collection":"tobira_odd","where":[["_id_field","==","stored"]]}`: 1,
		"{\"collection\":\"tobira_odd\",\"where\":[[\"`a.b`\",\"==\",7]]}":  1,
		`{"collection":"tobira_odd","where":[["a.b","==",8]]}`:              1,
		`{"collection":"tobira_odd","where":[["a.b","==",7]]}`:              0,
	} {
		if _, rows, _ := collect(t, c, driver.Query{Text: text}); len(rows) != want {
			t.Errorf("%s: %d rows, want %d", text, len(rows), want)
		}
	}
	b, _ := driver.As[driver.Browser](c)
	rs, err := b.Browse(ctxT(t), driver.CollectionRef{Name: "tobira_odd"}, driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{
		{Field: "a.b", Op: driver.OpEq, Value: driver.Int(7)},
		{Field: "`a`.b", Op: driver.OpEq, Value: driver.Int(8)},
		{Field: "_id_field", Op: driver.OpEq, Value: driver.String("stored")},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if rows, _, err := driver.Collect(ctxT(t), rs, 0); err != nil || len(rows) != 1 {
		t.Errorf("browse: %d rows, %v", len(rows), err)
	}
	m, _ := driver.As[driver.Mutator](c)
	if err := m.Update(ctxT(t), driver.CollectionRef{Name: "tobira_odd"}, driver.Object{{Key: "_id", Value: driver.String("x")}},
		driver.Object{{Key: "_id_field", Value: driver.String("changed")}}); err != nil {
		t.Fatal(err)
	}
	snap, err := coll.Doc("x").Get(ctxT(t))
	if err != nil || snap.Data()["_id"] != "changed" {
		t.Errorf("stored _id = %v, %v", snap.Data(), err)
	}
}

// A cursor is bound to its collection and filter.
func TestCursorBinding(t *testing.T) {
	c := openTest(t, testProject, false)
	b, _ := driver.As[driver.Browser](c)
	rs, err := b.Browse(ctxT(t), driver.CollectionRef{Name: "users/ada/orders"}, driver.BrowseOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, st, _ := driver.Collect(ctxT(t), rs, 0)
	for name, tt := range map[string]struct {
		ref  string
		opts driver.BrowseOptions
	}{
		"collection": {"users/grace/orders", driver.BrowseOptions{Limit: 1, Cursor: st.NextCursor}},
		"filter": {"users/ada/orders", driver.BrowseOptions{Limit: 1, Cursor: st.NextCursor,
			Filter: driver.Filter{Conditions: []driver.Condition{{Field: "total", Op: driver.OpGt, Value: driver.Int(0)}}}}},
	} {
		if _, err := b.Browse(ctxT(t), driver.CollectionRef{Name: tt.ref}, tt.opts); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("cursor on another %s: %v", name, err)
		}
	}
	rs, err = b.Browse(ctxT(t), driver.CollectionRef{Name: "users/ada/orders"}, driver.BrowseOptions{Limit: 5, Cursor: st.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, _ := driver.Collect(ctxT(t), rs, 0)
	if got := ids(t, rs.Columns(), rows); !slices.Equal(got, []string{"o2", "o3"}) {
		t.Errorf("next page = %v", got)
	}
}
