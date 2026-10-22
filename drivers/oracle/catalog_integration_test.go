//go:build integration

package oracle

import (
	"errors"
	"slices"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestNamespaces(t *testing.T) {
	c := openOra(t, false)
	nss, err := c.Namespaces(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]driver.Namespace{}
	for _, ns := range nss {
		got[ns.Name] = ns
	}
	for _, want := range []string{"TOBIRA", "TOBIRA_NS", "tobira_lower"} {
		if _, ok := got[want]; !ok {
			t.Errorf("Namespaces: %s missing from %v", want, nss)
		}
	}
	if !got["TOBIRA"].Default || got["TOBIRA_NS"].Default || got["TOBIRA"].Label != "schema" {
		t.Errorf("Default/Label: %+v", got["TOBIRA"])
	}
	for _, sys := range []string{"SYS", "SYSTEM", "XDB"} {
		if _, ok := got[sys]; ok {
			t.Errorf("Namespaces lists Oracle-maintained %s", sys)
		}
	}
}

func TestCollections(t *testing.T) {
	c := openOra(t, false)
	cols, err := c.Collections(ctxT(t), driver.NamespaceRef{})
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]driver.CollectionType{}
	for _, col := range cols {
		types[col.Ref.Name] = col.Type
		if col.Ref.Namespace != "TOBIRA" {
			t.Errorf("%s: namespace %q", col.Ref.Name, col.Ref.Namespace)
		}
		if col.Ref.Name == "tobira_people" && col.RowEstimate != 57 {
			t.Errorf("tobira_people row estimate %d, want 57 (stats gathered)", col.RowEstimate)
		}
		if col.Ref.Name == "TOBIRA_ORDERS" && col.Comment != "orders table" {
			t.Errorf("TOBIRA_ORDERS comment %q", col.Comment)
		}
	}
	for name, typ := range map[string]driver.CollectionType{"tobira_people": driver.CollectionTable, "tobira_people_view": driver.CollectionView, "TOBIRA_ORDERS": driver.CollectionTable} {
		if types[name] != typ {
			t.Errorf("%s: %q, want %q", name, types[name], typ)
		}
	}
	other, err := c.Collections(ctxT(t), driver.NamespaceRef{Name: "tobira_ns"}) // folds to TOBIRA_NS
	if err != nil || len(other) != 1 || other[0].Ref.Name != "THINGS" || other[0].Ref.Namespace != "TOBIRA_NS" {
		t.Errorf("Collections(tobira_ns): %v %v", other, err)
	}
	if _, err := c.Collections(ctxT(t), driver.NamespaceRef{Name: "no_such_schema"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("unknown schema: %v", err)
	}
}

func TestDescribe(t *testing.T) {
	c := openOra(t, false)
	s, err := c.Describe(ctxT(t), driver.CollectionRef{Name: "tobira_orders"}) // folds to TOBIRA_ORDERS
	if err != nil {
		t.Fatal(err)
	}
	if s.Ref != (driver.CollectionRef{Namespace: "TOBIRA", Name: "TOBIRA_ORDERS"}) || s.Comment != "orders table" {
		t.Errorf("ref/comment: %+v %q", s.Ref, s.Comment)
	}
	f := map[string]driver.Field{}
	for _, x := range s.Fields {
		f[x.Name] = x
	}
	if !f["ID"].AutoIncrement || f["ID"].Nullable {
		t.Errorf("ID: %+v", f["ID"])
	}
	if !f["DOUBLED"].Generated || f["DOUBLED"].NativeType != "NUMBER(9,2)" {
		t.Errorf("DOUBLED: %+v", f["DOUBLED"])
	}
	if f["NOTE"].Default != "'n/a'" || f["NOTE"].Comment != "free text" || f["NOTE"].NativeType != "VARCHAR2(50 CHAR)" {
		t.Errorf("NOTE: %+v", f["NOTE"])
	}
	if f["TOTAL"].Type != driver.TypeDecimal || f["PERSON_ID"].Type != driver.TypeInt || f["CREATED"].Type != driver.TypeTime {
		t.Errorf("types: %+v", s.Fields)
	}
	if !slices.Equal(s.PrimaryKey, []string{"ID"}) {
		t.Errorf("PK %v", s.PrimaryKey)
	}
	if len(s.ForeignKeys) != 1 || s.ForeignKeys[0].Name != "FK_ORDERS_PERSON" || s.ForeignKeys[0].References != (driver.CollectionRef{Namespace: "TOBIRA", Name: "tobira_people"}) ||
		!slices.Equal(s.ForeignKeys[0].Fields, []string{"PERSON_ID"}) || !slices.Equal(s.ForeignKeys[0].RefFields, []string{"id"}) {
		t.Errorf("FKs %+v", s.ForeignKeys)
	}
	idx := map[string]driver.Index{}
	for _, i := range s.Indexes {
		idx[i.Name] = i
	}
	if i := idx["TOBIRA_ORDERS_PERSON_TOTAL"]; !slices.Equal(i.Fields, []string{"PERSON_ID", "TOTAL"}) || i.Unique {
		t.Errorf("index: %+v", i)
	}
	if i := idx["TOBIRA_ORDERS_NOTE"]; !i.Unique {
		t.Errorf("unique index: %+v", i)
	}
	primary := 0
	for _, i := range s.Indexes {
		if i.Primary {
			primary++
		}
	}
	if primary != 1 {
		t.Errorf("want one primary index: %+v", s.Indexes)
	}

	v, err := c.Describe(ctxT(t), driver.CollectionRef{Name: "tobira_people_view"})
	if err != nil || v.Type != driver.CollectionView || len(v.Fields) != 2 {
		t.Errorf("view: %+v %v", v, err)
	}
	m, err := c.Describe(ctxT(t), driver.CollectionRef{Namespace: "tobira_lower", Name: "Mixed"})
	if err != nil || m.Ref.Namespace != "tobira_lower" || len(m.Fields) != 1 || m.Fields[0].Name != "Id" {
		t.Errorf("mixed-case names: %+v %v", m, err)
	}
	if _, err := c.Describe(ctxT(t), driver.CollectionRef{Name: "nope"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

// A namespaced query runs in that schema and leaves CURRENT_SCHEMA as it
// was for the next call on the same session.
func TestNamespacedQuery(t *testing.T) {
	c := openOra(t, false)
	c.db.SetMaxOpenConns(1)
	rows, _, _ := collect(t, c, driver.Query{Text: "select label from things", Namespace: "TOBIRA_NS"})
	if len(rows) != 1 || rows[0][0].String() != "in-ns" {
		t.Fatalf("namespaced: %v", rows)
	}
	if got := currentSchema(t, c); got != "TOBIRA" {
		t.Errorf("CURRENT_SCHEMA after a namespaced call = %s, want TOBIRA", got)
	}
	if err := queryErr(t, c, driver.Query{Text: "select 1 from dual", Namespace: "NO_SUCH_SCHEMA"}); err == nil {
		t.Error("unknown namespace: want an error")
	}
	if got := currentSchema(t, c); got != "TOBIRA" {
		t.Errorf("CURRENT_SCHEMA after a failed namespaced call = %s, want TOBIRA", got)
	}
}

// A writable statement that changes the session does not leak into later
// calls: its session is discarded.
func TestWritableSessionDiscarded(t *testing.T) {
	c := openOra(t, false)
	c.db.SetMaxOpenConns(1)
	rs, err := c.Query(ctxT(t), driver.Query{Text: "alter session set current_schema = tobira_ns"})
	if err != nil {
		t.Fatal(err)
	}
	_ = rs.Close()
	if got := currentSchema(t, c); got != "TOBIRA" {
		t.Errorf("CURRENT_SCHEMA leaked to the next call: %s", got)
	}
	_, st, _ := collect(t, c, driver.Query{Text: "update tobira_ns.things set label = label"})
	if st.RowsAffected != 1 {
		t.Errorf("RowsAffected: %d", st.RowsAffected)
	}
}
