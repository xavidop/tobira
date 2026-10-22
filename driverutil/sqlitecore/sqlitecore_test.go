package sqlitecore

import (
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestGuardsCarryTheDriverName(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		kind error
	}{
		{"CheckReadOnly", CheckReadOnly("d1", "delete from t"), driver.ErrReadOnly},
		{"CheckSingle", CheckSingle("d1", "select 1; select 2"), driver.ErrSyntax},
		{"CheckExplain", CheckExplain("d1", "pragma user_version = 3"), driver.ErrReadOnly},
		{"CheckNoWrite", CheckNoWrite("d1", "with x as (select 1) delete from t"), driver.ErrReadOnly},
		{"ReadOnlyGuard", ReadOnlyGuard("d1", "select load_extension('x')"), driver.ErrReadOnly},
	} {
		var de *driver.Error
		if !errors.As(tc.err, &de) || de.Driver != "d1" || !errors.Is(tc.err, tc.kind) {
			t.Errorf("%s: got %v, want a d1 %v", tc.name, tc.err, tc.kind)
		}
	}
}

func TestReadOnlyGuard(t *testing.T) {
	allowed := []string{
		"select * from t",
		"select replace(name, 'a', 'b') from t",
		"pragma table_info(t)",
		"explain query plan select 1",
		"with x as (select 1) select * from x",
		"select 'delete' as \"update\"",
	}
	for _, q := range allowed {
		if err := ReadOnlyGuard("d1", q); err != nil {
			t.Errorf("ReadOnlyGuard(%q) = %v, want nil", q, err)
		}
	}
	denied := []string{
		"insert into t values (1)",
		"with x as (select 1) insert into t select * from x",
		"with x as (select 1) replace into t select * from x",
		"attach database 'x' as y",
		"pragma user_version = 3",
		"pragma writable_schema",
		"select \"load_extension\"('x')",
		"vacuum into '/tmp/x'",
		"create table t (a)",
		"",
	}
	for _, q := range denied {
		if err := ReadOnlyGuard("d1", q); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("ReadOnlyGuard(%q) = %v, want ErrReadOnly", q, err)
		}
	}
}

func TestBrowseQuery(t *testing.T) {
	schema := &driver.CollectionSchema{
		Ref:        driver.CollectionRef{Namespace: "main", Name: "people"},
		Type:       driver.CollectionTable,
		Fields:     []driver.Field{{Name: "id"}, {Name: "Name"}},
		PrimaryKey: []string{"id"},
	}
	opts := driver.BrowseOptions{
		Cursor: "20", Limit: 10, ReadOnly: true,
		Filter: driver.Filter{Conditions: []driver.Condition{{Field: "name", Op: driver.OpPrefix, Value: driver.String("a_%")}}},
		Sort:   []driver.SortKey{{Field: "NAME", Desc: true}},
	}
	page, err := ParseBrowse("d1", opts)
	if err != nil || page != (BrowsePage{Offset: 20, Limit: 10}) {
		t.Fatalf("ParseBrowse = %+v, %v", page, err)
	}
	q, err := BrowseQuery("d1", schema, opts, page)
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT * FROM "main"."people" WHERE "Name" LIKE ?||'%' ESCAPE '\' ORDER BY "Name" DESC NULLS FIRST, "id" LIMIT ? OFFSET ?`
	if q.Text != want {
		t.Errorf("text\n got %s\nwant %s", q.Text, want)
	}
	if len(q.Params) != 3 || q.Params[0].String() != `a\_\%` || q.Limit != 10 || !q.ReadOnly {
		t.Errorf("query = %+v", q)
	}
	if _, err := ParseBrowse("d1", driver.BrowseOptions{Cursor: "-1"}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("bad cursor: %v", err)
	}
	if p, _ := ParseBrowse("d1", driver.BrowseOptions{}); p.Limit != DefaultBrowseLimit {
		t.Errorf("default limit = %d", p.Limit)
	}
	opts.Sort = []driver.SortKey{{Field: "nope"}}
	if _, err := BrowseQuery("d1", schema, opts, page); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("unknown sort field: %v", err)
	}
}

// TestCatalogParsersTolerateHostileRows feeds short rows and wrong value
// types: nothing panics, and well-formed rows still parse.
func TestCatalogParsersTolerateHostileRows(t *testing.T) {
	hostile := [][]any{nil, {}, {"x"}, {1.5, []byte("b"), nil, "z", map[string]any{}, true, nil}}
	if got := Collections("main", hostile); len(got) != 1 {
		t.Errorf("Collections kept %d rows, want the 1 with two values", len(got))
	}
	s := &driver.CollectionSchema{Ref: driver.CollectionRef{Namespace: "main", Name: "t"}, Type: driver.CollectionTable, DDL: "CREATE TABLE t (id INTEGER PRIMARY KEY, v)"}
	ApplyTableXInfo(s, append(hostile,
		[]any{int64(0), "id", "INTEGER", int64(0), nil, int64(1), int64(0)},
		[]any{int64(1), "v", "", int64(1), "'d'", int64(0), int64(0)},
		[]any{int64(2), "h", "", int64(0), nil, int64(0), int64(1)}, // hidden
	))
	if len(s.PrimaryKey) != 1 || s.PrimaryKey[0] != "id" || !s.Fields[len(s.Fields)-2].AutoIncrement {
		t.Errorf("ApplyTableXInfo: %+v", s)
	}
	if f := s.Fields[len(s.Fields)-1]; f.Name != "v" || f.Nullable || f.Default != "'d'" {
		t.Errorf("field v = %+v", f)
	}
	if idx := IndexList(hostile); len(idx) != 1 {
		t.Errorf("IndexList = %+v", idx)
	}
	if cols := IndexColumns([][]any{{int64(0), int64(1), "a"}, {int64(1), int64(-2), nil}, {"short"}}); len(cols) != 1 || cols[0] != "a" {
		t.Errorf("IndexColumns = %q", cols)
	}
	ApplyForeignKeys(s, append(hostile,
		[]any{int64(0), int64(0), "p", "a", "x"},
		[]any{int64(0), int64(1), "p", "b", nil},
	))
	// The hostile row's id (1.5) reads as 0, so it joins key 0.
	if n := len(s.ForeignKeys); n != 1 || len(s.ForeignKeys[0].Fields) != 3 || len(s.ForeignKeys[0].RefFields) != 2 {
		t.Errorf("ApplyForeignKeys = %+v", s.ForeignKeys)
	}
}

func TestMutationSQL(t *testing.T) {
	schema := &driver.CollectionSchema{Ref: driver.CollectionRef{Name: "t"}, Fields: []driver.Field{{Name: "id"}, {Name: "A"}, {Name: "a"}, {Name: "b\"q"}}}
	where, params, err := WhereKey("d1", "update", schema, driver.Object{{Key: "id", Value: driver.Int(1)}, {Key: "B\"Q", Value: driver.Null()}})
	if err != nil || where != ` WHERE "id" = ? AND "b""q" IS NULL` || len(params) != 1 {
		t.Errorf("WhereKey = %q, %v, %v", where, params, err)
	}
	if _, _, err := WhereKey("d1", "update", schema, nil); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("empty key: %v", err)
	}
	if col, err := Column("d1", "insert", schema, "a"); err != nil || col != `"a"` {
		t.Errorf("exact match = %q, %v", col, err)
	}
	if _, err := Column("d1", "insert", schema, "ID2"); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	if _, err := Column("d1", "insert", &driver.CollectionSchema{Fields: []driver.Field{{Name: "A"}, {Name: "a"}}}, "A"); err != nil {
		t.Errorf("exact over folded: %v", err)
	}
	if _, err := Column("d1", "insert", &driver.CollectionSchema{Fields: []driver.Field{{Name: "Ab"}, {Name: "aB"}}}, "ab"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("ambiguous: %v", err)
	}
}

func TestBuildPlanDepthCapped(t *testing.T) {
	var rows []driver.Row
	for i := 1; i <= 1000; i++ {
		rows = append(rows, driver.Row{driver.Int(int64(i)), driver.Int(int64(i - 1)), driver.Int(0), driver.String("SCAN t")})
	}
	p := BuildPlan(rows, "select 1")
	if len(p.Text) > 1000*(2*MaxPlanDepth+len("SCAN t\n")) {
		t.Errorf("plan text is %d bytes", len(p.Text))
	}
	if got := BuildPlan(nil, "insert into t values (1)").Text; got != NoPlanRowsWrite {
		t.Errorf("empty write plan = %q", got)
	}
	if got := BuildPlan(nil, "pragma user_version").Text; got != NoPlanRows {
		t.Errorf("empty plan = %q", got)
	}
}

func TestTypes(t *testing.T) {
	if MapType("decimal(10, 2)") != driver.TypeDecimal || MapType("") != driver.TypeString {
		t.Error("MapType")
	}
	if ColumnType("", int64(1)) != driver.TypeInt || ColumnType("", []byte{1}) != driver.TypeBytes {
		t.Error("ColumnType")
	}
	if v := ConvertValue("BOOLEAN", int64(1)); v.Type() != driver.TypeBool {
		t.Errorf("ConvertValue bool = %v", v)
	}
	if v := ConvertValue("DECIMAL(10,2)", "12.5"); v.String() != "12.50" {
		t.Errorf("ConvertValue decimal = %v", v)
	}
}

// TestBuildPlanBelowCap: under MaxPlanDepth the tree and text are the plain
// parent/child nesting the sqlite driver always produced (an unknown parent
// attaches to the root).
func TestBuildPlanBelowCap(t *testing.T) {
	row := func(id, parent int64, detail string) driver.Row {
		return driver.Row{driver.Int(id), driver.Int(parent), driver.Int(0), driver.String(detail)}
	}
	rows := []driver.Row{row(2, 0, "SCAN a"), row(3, 2, "SEARCH b"), row(4, 3, "USE TEMP B-TREE"), row(5, 0, "SCAN c"), row(6, 99, "ORPHAN")}
	for d := int64(0); d < 60; d++ {
		rows = append(rows, row(100+d, 99+d, "D"))
	}
	rows[len(rows)-60] = row(100, 5, "D") // chain of 60 under "SCAN c"
	p := BuildPlan(rows, "select 1")
	var want strings.Builder
	want.WriteString("SCAN a\n  SEARCH b\n    USE TEMP B-TREE\nSCAN c\nORPHAN\n")
	for d := 0; d < 60; d++ {
		want.WriteString(strings.Repeat("  ", d+1) + "D\n")
	}
	if p.Text != want.String() {
		t.Errorf("text\n%s\nwant\n%s", p.Text, want.String())
	}
	if len(p.Root.Children) != 3 || p.Root.Children[0].Children[0].Children[0].Label != "USE TEMP B-TREE" {
		t.Errorf("tree = %+v", p.Root)
	}
}
