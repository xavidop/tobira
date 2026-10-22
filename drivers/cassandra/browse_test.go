package cassandra

import (
	"errors"
	"slices"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func peopleSchema() *driver.CollectionSchema {
	return &driver.CollectionSchema{
		Ref: driver.CollectionRef{Namespace: "ks", Name: "people"},
		Fields: []driver.Field{
			{Name: "id", NativeType: "int", Type: driver.TypeInt},
			{Name: "age", NativeType: "int", Type: driver.TypeInt},
			{Name: "tags", NativeType: "frozen<list<text>>", Type: driver.TypeList},
			{Name: "attrs", NativeType: "map<text, text>", Type: driver.TypeObject},
		},
		PrimaryKey: []string{"id"},
	}
}

func TestBuildSelect(t *testing.T) {
	sch := peopleSchema()
	text, params, err := buildSelect(sch, driver.Filter{
		Conditions: []driver.Condition{
			{Field: "age", Op: driver.OpGe, Value: driver.Int(30)},
			{Field: "id", Op: driver.OpIn, Value: driver.List(driver.Int(1), driver.Int(2))},
			{Field: "tags", Op: driver.OpContains, Value: driver.String("beta")},
			{Field: "attrs", Op: driver.OpContains, Value: driver.String("x")},
		},
		Raw: "age < 60",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT * FROM "ks"."people" WHERE "age" >= ? AND "id" IN (?, ?) AND "tags" CONTAINS ? AND "attrs" CONTAINS ? AND age < 60` + "\n" + ` ALLOW FILTERING`
	if text != want {
		t.Errorf("text =\n%s\nwant\n%s", text, want)
	}
	if len(params) != 5 || !drivertest.EqualValue(driver.Int(30), params[0].v) || !drivertest.EqualValue(driver.String("x"), params[4].v) {
		t.Errorf("params = %v", params)
	}
	text, params, err = buildSelect(sch, driver.Filter{})
	if err != nil || text != `SELECT * FROM "ks"."people"` || len(params) != 0 {
		t.Errorf("no filter: %q %v %v", text, params, err)
	}
	text, _, err = buildSelect(sch, driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpEq, Value: driver.Int(1)}}})
	if err != nil || text != `SELECT * FROM "ks"."people" WHERE "id" = ? ALLOW FILTERING` {
		t.Errorf("eq: %q %v", text, err)
	}
}

func TestBuildSelectRefuses(t *testing.T) {
	sch := peopleSchema()
	for _, tc := range []struct {
		f    driver.Filter
		kind error
	}{
		{driver.Filter{Conditions: []driver.Condition{{Field: "age", Op: driver.OpNe, Value: driver.Int(1)}}}, driver.ErrUnsupported},
		{driver.Filter{Conditions: []driver.Condition{{Field: "age", Op: driver.OpIsNull}}}, driver.ErrUnsupported},
		{driver.Filter{Conditions: []driver.Condition{{Field: "age", Op: driver.OpNotNull}}}, driver.ErrUnsupported},
		{driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpContains, Value: driver.String("1")}}}, driver.ErrUnsupported},
		{driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpPrefix, Value: driver.String("1")}}}, driver.ErrUnsupported},
		{driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpIn, Value: driver.Int(1)}}}, driver.ErrSyntax},
		{driver.Filter{Conditions: []driver.Condition{{Field: "nope", Op: driver.OpEq, Value: driver.Int(1)}}}, driver.ErrNotFound},
		{driver.Filter{Conditions: []driver.Condition{{Field: "age", Op: "bogus", Value: driver.Int(1)}}}, driver.ErrUnsupported},
		{driver.Filter{Raw: "age > 1; DROP TABLE t"}, driver.ErrSyntax},
	} {
		if _, _, err := buildSelect(sch, tc.f); !errors.Is(err, tc.kind) {
			t.Errorf("buildSelect(%+v) = %v, want %v", tc.f, err, tc.kind)
		}
	}
}

func TestBrowseCursor(t *testing.T) {
	tag := cursorTag("SELECT * FROM t", nil, nil)
	for _, c := range []browseCursor{{}, {state: []byte{0, 1, 0xfe}, tag: tag}, {offset: 40, tag: tag}} {
		got, err := decodeCursor(c.encode())
		if err != nil || got.offset != c.offset || !slices.Equal(got.state, c.state) {
			t.Errorf("round trip %+v -> %q -> %+v, %v", c, c.encode(), got, err)
		}
		if c.encode() != "" && got.tag != tag {
			t.Errorf("round trip lost the tag: %+v", got)
		}
	}
	if (browseCursor{}).encode() != "" {
		t.Error("the first page's cursor must be empty")
	}
	for _, bad := range []string{"x:1", "o:-1", "o:x", "p:!!", "p:", "o:1", "p:AAEC", "o:zz:1"} {
		if _, err := decodeCursor(bad); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("decodeCursor(%q) = %v, want ErrSyntax", bad, err)
		}
	}
}

// A cursor is bound to the statement, values and sort it came from.
func TestCursorTag(t *testing.T) {
	base := cursorTag(`SELECT * FROM "ks"."t" WHERE "a" = ? ALLOW FILTERING`, []param{{driver.Int(1)}}, nil)
	if base != cursorTag(`SELECT * FROM "ks"."t" WHERE "a" = ? ALLOW FILTERING`, []param{{driver.Int(1)}}, nil) {
		t.Error("the tag is not deterministic")
	}
	for name, other := range map[string]uint64{
		"table":  cursorTag(`SELECT * FROM "ks"."u" WHERE "a" = ? ALLOW FILTERING`, []param{{driver.Int(1)}}, nil),
		"value":  cursorTag(`SELECT * FROM "ks"."t" WHERE "a" = ? ALLOW FILTERING`, []param{{driver.Int(2)}}, nil),
		"type":   cursorTag(`SELECT * FROM "ks"."t" WHERE "a" = ? ALLOW FILTERING`, []param{{driver.String("1")}}, nil),
		"sort":   cursorTag(`SELECT * FROM "ks"."t" WHERE "a" = ? ALLOW FILTERING`, []param{{driver.Int(1)}}, []driver.SortKey{{Field: "a"}}),
		"filter": cursorTag(`SELECT * FROM "ks"."t"`, nil, nil),
	} {
		if other == base {
			t.Errorf("a different %s gives the same tag", name)
		}
	}
	cur := browseCursor{offset: 20, tag: base}
	if err := cur.check(base); err != nil {
		t.Errorf("matching tag: %v", err)
	}
	if err := cur.check(base + 1); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("mismatched tag: %v, want ErrSyntax", err)
	}
	if err := (browseCursor{}).check(base); err != nil {
		t.Errorf("the first page has no tag to check: %v", err)
	}
}

func TestSortRows(t *testing.T) {
	cols := []driver.Column{{Name: "id"}, {Name: "age"}}
	rows := []driver.Row{
		{driver.Int(1), driver.Int(30)},
		{driver.Int(2), driver.Null()},
		{driver.Int(3), driver.Int(20)},
		{driver.Int(4), driver.Int(30)},
	}
	if err := sortRows(cols, rows, []driver.SortKey{{Field: "age", Desc: true}, {Field: "id"}}); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, r := range rows {
		n, _ := r[0].AsInt()
		ids = append(ids, n)
	}
	if want := []int64{1, 4, 3, 2}; !slices.Equal(ids, want) {
		t.Errorf("sorted ids = %v, want %v (NULLs last)", ids, want)
	}
	if err := sortRows(cols, rows, []driver.SortKey{{Field: "nope"}}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("unknown sort field: %v, want ErrNotFound", err)
	}
}
