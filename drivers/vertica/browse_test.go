package vertica

import (
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestParseOffsetCursor(t *testing.T) {
	for in, want := range map[string]int{"": 0, "o:0": 0, "o:40": 40} {
		got, err := parseOffsetCursor(in)
		if err != nil || got != want {
			t.Errorf("parseOffsetCursor(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"40", "o:-1", "o:x", "x:1", "o:" + strings.Repeat("9", 80)} {
		if _, err := parseOffsetCursor(in); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("parseOffsetCursor(%q) = %v, want ErrSyntax", in, err)
		}
	}
}

func peopleSchema() *driver.CollectionSchema {
	return &driver.CollectionSchema{
		Ref:  driver.CollectionRef{Namespace: "public", Name: "tobira_people"},
		Type: driver.CollectionTable,
		Fields: []driver.Field{
			{Name: "id", NativeType: "int", Type: driver.TypeInt},
			{Name: "name", NativeType: "varchar(255)", Type: driver.TypeString},
			{Name: "score", NativeType: "numeric(10,2)", Type: driver.TypeDecimal},
			{Name: "big", NativeType: "numeric(37,15)", Type: driver.TypeDecimal},
		},
		PrimaryKey: []string{"id"},
	}
}

func TestBrowseSQL(t *testing.T) {
	q, wide, err := browseSQL(peopleSchema(), driver.BrowseOptions{
		Limit: 20,
		Sort:  []driver.SortKey{{Field: "NAME", Desc: true}},
		Filter: driver.Filter{Conditions: []driver.Condition{
			{Field: "score", Op: driver.OpGt, Value: driver.Decimal("1.5")},
			{Field: "name", Op: driver.OpContains, Value: driver.String("a%_b")},
			{Field: "id", Op: driver.OpIn, Value: driver.List(driver.Int(1), driver.Int(2))},
		}},
	}, 40)
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT "id", "name", "score", "big"::VARCHAR AS "big" FROM "public"."tobira_people"` +
		` WHERE "score" > ? AND STRPOS("name"::VARCHAR, ?) > 0 AND "id" IN (?, ?)` +
		` ORDER BY CASE WHEN "name" IS NULL THEN 0 ELSE 1 END, "name" DESC, "id" LIMIT 21 OFFSET 40`
	if q.Text != want {
		t.Errorf("browseSQL =\n  %s\nwant\n  %s", q.Text, want)
	}
	if len(q.Params) != 4 || q.Limit != 20 {
		t.Errorf("browseSQL params = %v, limit %d", q.Params, q.Limit)
	}
	if len(wide) != 4 || !wide[3] || wide[2] {
		t.Errorf("browseSQL wide decimals = %v, want only the 4th column", wide)
	}

	q, _, err = browseSQL(peopleSchema(), driver.BrowseOptions{Filter: driver.Filter{Raw: "id < 3 -- c"}}, 0)
	if err != nil || !strings.Contains(q.Text, "WHERE (id < 3 -- c\n)") || !strings.HasSuffix(q.Text, "LIMIT 101 OFFSET 0") {
		t.Errorf("browseSQL(raw) = %q, %v", q.Text, err)
	}

	for _, opts := range []driver.BrowseOptions{
		{Sort: []driver.SortKey{{Field: "nope"}}},
		{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "nope", Op: driver.OpEq, Value: driver.Int(1)}}}},
	} {
		if _, _, err := browseSQL(peopleSchema(), opts, 0); !errors.Is(err, driver.ErrNotFound) {
			t.Errorf("browseSQL(%+v) = %v, want ErrNotFound", opts, err)
		}
	}
	if _, _, err := browseSQL(peopleSchema(), driver.BrowseOptions{Filter: driver.Filter{Raw: "1=1) OR (1=1"}}, 0); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("browseSQL(bad raw) = %v, want ErrSyntax", err)
	}
}
