package mysql

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func peopleSchema() *driver.CollectionSchema {
	return &driver.CollectionSchema{
		Ref: driver.CollectionRef{Namespace: "db", Name: "people"},
		Fields: []driver.Field{
			{Name: "id", Type: driver.TypeInt}, {Name: "name", Type: driver.TypeString},
			{Name: "Name", Type: driver.TypeString}, {Name: "active", Type: driver.TypeBool},
			{Name: "at", Type: driver.TypeTime}, {Name: "score", Type: driver.TypeDecimal},
		},
		PrimaryKey: []string{"id"},
	}
}

func TestKeysetCursorRoundTrip(t *testing.T) {
	s := peopleSchema()
	for _, pk := range [][]string{{"id"}, {"active", "at", "score", "name"}} {
		s.PrimaryKey = pk
		var key []driver.Value
		for _, name := range pk {
			switch schemaField(s, name).Type {
			case driver.TypeInt:
				key = append(key, driver.Int(42))
			case driver.TypeBool:
				key = append(key, driver.Bool(true))
			case driver.TypeTime:
				key = append(key, driver.Time(time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC)))
			case driver.TypeDecimal:
				key = append(key, driver.Decimal("12.50"))
			default:
				key = append(key, driver.String("a`b"))
			}
		}
		cur, err := encodeKeysetCursor(key)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeKeysetCursor(cur, s)
		if err != nil {
			t.Fatalf("decode %v: %v", pk, err)
		}
		for i := range key {
			if got[i].Type() != key[i].Type() || got[i].String() != key[i].String() {
				t.Errorf("%v: key %d = %s (%s), want %s (%s)", pk, i, got[i], got[i].Type(), key[i], key[i].Type())
			}
		}
	}
}

func TestKeysetCursorRejectsGarbage(t *testing.T) {
	s := peopleSchema()
	for _, cur := range []string{"x", "k:!!", "k:" + strings.Repeat("A", maxCursorLen), "k:W10", "k:WyJhIl0", "k:W251bGxd"} {
		if _, err := decodeKeysetCursor(cur, s); err == nil {
			t.Errorf("decodeKeysetCursor(%q) succeeded", cur)
		}
	}
	for _, cur := range []string{"x", "o:-1", "o:a", "1"} {
		if _, err := parseOffsetCursor(cur); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("parseOffsetCursor(%q) = %v, want ErrSyntax", cur, err)
		}
	}
}

func TestBrowseConditions(t *testing.T) {
	for _, tc := range []struct {
		cond   driver.Condition
		clause string
		params int
	}{
		{driver.Condition{Field: "id", Op: driver.OpEq, Value: driver.Int(1)}, "`id` = ?", 1},
		{driver.Condition{Field: "id", Op: driver.OpEq, Value: driver.Null()}, "`id` IS NULL", 0},
		{driver.Condition{Field: "id", Op: driver.OpNe, Value: driver.Null()}, "`id` IS NOT NULL", 0},
		{driver.Condition{Field: "id", Op: driver.OpIn, Value: driver.List()}, "FALSE", 0},
		{driver.Condition{Field: "id", Op: driver.OpIn, Value: driver.List(driver.Int(1), driver.Int(2))}, "`id` IN (?, ?)", 2},
		{driver.Condition{Field: "name", Op: driver.OpContains, Value: driver.String("a%_!")}, "CAST(`name` AS CHAR) LIKE CONCAT('%', ?, '%') ESCAPE '!'", 1},
		{driver.Condition{Field: "name", Op: driver.OpPrefix, Value: driver.String("a")}, "CAST(`name` AS CHAR) LIKE CONCAT(?, '%') ESCAPE '!'", 1},
	} {
		b := &browseQuery{op: "browse", schema: peopleSchema()}
		col, err := b.field(tc.cond.Field)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.addCondition(col, tc.cond); err != nil {
			t.Fatalf("%+v: %v", tc.cond, err)
		}
		if b.where[0] != tc.clause || len(b.params) != tc.params {
			t.Errorf("%+v: %q with %d params, want %q with %d", tc.cond, b.where[0], len(b.params), tc.clause, tc.params)
		}
		if tc.cond.Op == driver.OpContains && b.params[0].String() != "a!%!_!!" {
			t.Errorf("LIKE escaping: %s", b.params[0])
		}
	}
	b := &browseQuery{op: "browse", schema: peopleSchema()}
	if _, err := b.field("nope"); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("unknown field: %v", err)
	}
	if col, err := b.field("Name"); err != nil || col != "`Name`" {
		t.Errorf("exact match wins: %q, %v", col, err)
	}
	if _, err := b.field("NAME"); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("ambiguous field: %v", err)
	}
	if err := b.addCondition("`id`", driver.Condition{Op: driver.OpEq, Value: driver.List()}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("list value: %v", err)
	}
	if err := b.addCondition("`id`", driver.Condition{Op: "regex", Value: driver.Int(1)}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("unknown op: %v", err)
	}
}

func TestIsolationLevel(t *testing.T) {
	for in, want := range map[string]string{"": "", "serializable": "SERIALIZABLE", " read  committed ": "READ COMMITTED", "Repeatable Read": "REPEATABLE READ", "read uncommitted": "READ UNCOMMITTED"} {
		if got, ok := isolationLevel(in); !ok || got != want {
			t.Errorf("isolationLevel(%q) = %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{"snapshot", "READ COMMITTED; COMMIT"} {
		if _, ok := isolationLevel(in); ok {
			t.Errorf("isolationLevel(%q) accepted", in)
		}
	}
}

func TestParsePlan(t *testing.T) {
	raw := `{"query_block": {"select_id": 1, "cost_info": {"query_cost": "2.50"},
	  "ordering_operation": {"using_filesort": true, "nested_loop": [
	    {"table": {"table_name": "a", "access_type": "ALL", "rows_examined_per_scan": 10, "cost_info": {"prefix_cost": "1.00"}}},
	    {"table": {"table_name": "b", "access_type": "eq_ref", "key": "PRIMARY", "rows_examined_per_scan": 1}}
	  ]}}}`
	root, err := parsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	if root.Label != "query_block" || root.Cost != 2.5 || len(root.Children) != 1 {
		t.Fatalf("root = %+v", root)
	}
	ord := root.Children[0]
	if ord.Label != "ordering_operation" || len(ord.Children) != 2 {
		t.Fatalf("ordering = %+v", ord)
	}
	a, b := ord.Children[0], ord.Children[1]
	if a.Label != "table" || a.Detail != "a ALL" || a.Rows != 10 || a.Cost != 1 {
		t.Errorf("a = %+v", a)
	}
	if b.Detail != "b eq_ref PRIMARY" || b.Rows != 1 {
		t.Errorf("b = %+v", b)
	}
	if _, err := parsePlan("not json"); err == nil {
		t.Error("parsePlan(garbage) succeeded")
	}
	if _, err := parsePlan("{}"); err == nil {
		t.Error("parsePlan({}) succeeded")
	}
}

func TestRenderTable(t *testing.T) {
	cols := []driver.Column{{Name: "id"}, {Name: "table"}, {Name: "key"}}
	rows := []driver.Row{{driver.Int(1), driver.String("people"), driver.Null()}}
	got := renderTable(cols, rows)
	want := "id  table   key\n1   people  NULL"
	if got != want {
		t.Errorf("renderTable =\n%s\nwant\n%s", got, want)
	}
}

func TestPageResultSetMapsTinyint1(t *testing.T) {
	inner := driver.NewSliceResultSet(
		[]driver.Column{{Name: "id", Type: driver.TypeInt}, {Name: "active", Type: driver.TypeInt}},
		[]driver.Row{{driver.Int(1), driver.Int(1)}, {driver.Int(2), driver.Int(0)}, {driver.Int(3), driver.Int(7)}, {driver.Int(4), driver.Null()}},
		driver.Stats{})
	p := &pageResultSet{ResultSet: inner, cols: []driver.Column{{Name: "id", Type: driver.TypeInt}, {Name: "active", Type: driver.TypeBool}}, boolIdx: []int{1}}
	rows, _, err := driver.Collect(t.Context(), p, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r[1].Type().String()+":"+r[1].String())
	}
	want := []string{"bool:true", "bool:false", "int:7", "null:NULL"}
	if !slices.Equal(got, want) {
		t.Errorf("active = %v, want %v", got, want)
	}
}
