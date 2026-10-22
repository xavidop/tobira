package oracle

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func peopleSchema() *driver.CollectionSchema {
	return &driver.CollectionSchema{
		Ref:  driver.CollectionRef{Namespace: "TOBIRA", Name: "tobira_people"},
		Type: driver.CollectionTable,
		Fields: []driver.Field{
			{Name: "id", Type: driver.TypeInt}, {Name: "name", Type: driver.TypeString},
			{Name: "age", Type: driver.TypeInt}, {Name: "active", Type: driver.TypeBool},
			{Name: "created_at", Type: driver.TypeTime},
		},
		PrimaryKey: []string{"id"},
	}
}

func TestBuildBrowseKeyset(t *testing.T) {
	b, err := buildBrowse(peopleSchema(), driver.BrowseOptions{Limit: 20}, 20)
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT * FROM "TOBIRA"."tobira_people" ORDER BY "id" FETCH FIRST 21 ROWS ONLY`
	if b.text != want || !b.keyset {
		t.Errorf("first page:\n got %s (keyset %v)\nwant %s", b.text, b.keyset, want)
	}
	cur, err := encodeKeysetCursor([]driver.Value{driver.Int(20)})
	if err != nil {
		t.Fatal(err)
	}
	b, err = buildBrowse(peopleSchema(), driver.BrowseOptions{Limit: 20, Cursor: cur}, 20)
	if err != nil {
		t.Fatal(err)
	}
	want = `SELECT * FROM "TOBIRA"."tobira_people" WHERE ("id" > :1) ORDER BY "id" FETCH FIRST 21 ROWS ONLY`
	if b.text != want || len(b.params) != 1 || b.params[0].String() != "20" {
		t.Errorf("next page:\n got %s %v\nwant %s", b.text, b.params, want)
	}
}

func TestBuildBrowseCompositeKeyset(t *testing.T) {
	s := peopleSchema()
	s.PrimaryKey = []string{"id", "name"}
	cur, _ := encodeKeysetCursor([]driver.Value{driver.Int(3), driver.String("x")})
	b, err := buildBrowse(s, driver.BrowseOptions{Limit: 5, Cursor: cur}, 5)
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT * FROM "TOBIRA"."tobira_people" WHERE (("id" > :1) OR ("id" = :2 AND "name" > :3)) ORDER BY "id", "name" FETCH FIRST 6 ROWS ONLY`
	if b.text != want || len(b.params) != 3 {
		t.Errorf("composite:\n got %s %v\nwant %s", b.text, b.params, want)
	}
}

func TestBuildBrowseSortAndFilter(t *testing.T) {
	b, err := buildBrowse(peopleSchema(), driver.BrowseOptions{
		Limit:  10,
		Cursor: "o:20",
		Sort:   []driver.SortKey{{Field: "age", Desc: true}, {Field: "NAME"}},
		Filter: driver.Filter{
			Conditions: []driver.Condition{
				{Field: "active", Op: driver.OpEq, Value: driver.Bool(true)},
				{Field: "name", Op: driver.OpContains, Value: driver.String("a%_!")},
				{Field: "age", Op: driver.OpIn, Value: driver.List(driver.Int(1), driver.Int(2))},
				{Field: "age", Op: driver.OpIsNull},
				{Field: "created_at", Op: driver.OpGe, Value: driver.Time(time.Unix(0, 0))},
				{Field: "name", Op: driver.OpPrefix, Value: driver.String("Ad")},
				{Field: "name", Op: driver.OpNe, Value: driver.Null()},
			},
			Raw: "age > 3",
		},
	}, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT * FROM "TOBIRA"."tobira_people" WHERE "active" = :1 AND TO_CHAR("name") LIKE '%' || :2 || '%' ESCAPE '!' AND "age" IN (:3, :4) AND "age" IS NULL AND "created_at" >= :5 AND TO_CHAR("name") LIKE :6 || '%' ESCAPE '!' AND "name" IS NOT NULL AND (age > 3` + "\n" + `) ORDER BY "age" DESC NULLS FIRST, "name" ASC NULLS LAST, "id" OFFSET 20 ROWS FETCH NEXT 11 ROWS ONLY`
	if b.text != want || b.keyset || b.offset != 20 {
		t.Errorf("sort/filter:\n got %s\nwant %s", b.text, want)
	}
	if got := b.params[1].String(); got != "a!%!_!!" {
		t.Errorf("LIKE escaping: got %q", got)
	}
}

func TestBuildBrowseNoKey(t *testing.T) {
	s := peopleSchema()
	s.PrimaryKey = nil
	b, err := buildBrowse(s, driver.BrowseOptions{Limit: 2}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.text, `ORDER BY ROWID OFFSET 0 ROWS FETCH NEXT 3 ROWS ONLY`) || b.keyset {
		t.Errorf("no key: %s", b.text)
	}
	s.Type = driver.CollectionView
	b, err = buildBrowse(s, driver.BrowseOptions{Limit: 2}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.text, `ORDER BY "id", "name", "age", "active", "created_at" OFFSET 0`) {
		t.Errorf("view without key: %s", b.text)
	}
}

func TestBuildBrowseRefuses(t *testing.T) {
	for _, opts := range []driver.BrowseOptions{
		{Cursor: "o:-1"},
		{Cursor: "x"},
		{Cursor: "k:!!"},
		{Cursor: "o:" + strings.Repeat("9", 5000)},
		{Sort: []driver.SortKey{{Field: "nope"}}},
		{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "nope", Op: driver.OpEq, Value: driver.Int(1)}}}},
		{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: "regex", Value: driver.Int(1)}}}},
		{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpIn, Value: driver.Int(1)}}}},
		{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpEq, Value: driver.List()}}}},
		{Filter: driver.Filter{Raw: "1 = 1) or (1 = 1"}},
	} {
		if _, err := buildBrowse(peopleSchema(), opts, 10); err == nil {
			t.Errorf("buildBrowse(%+v): want an error", opts)
		} else {
			var de *driver.Error
			if !errors.As(err, &de) || de.Driver != Name {
				t.Errorf("buildBrowse(%+v): error %v is not a driver error", opts, err)
			}
		}
	}
	empty := driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpIn, Value: driver.List()}}}}
	b, err := buildBrowse(peopleSchema(), empty, 10)
	if err != nil || !strings.Contains(b.text, "WHERE 1 = 0") {
		t.Errorf("empty IN: %v %s", err, b.text)
	}
}

func TestKeysetCursorRoundTrip(t *testing.T) {
	s := peopleSchema()
	s.PrimaryKey = []string{"id", "name", "created_at"}
	ts := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	cur, err := encodeKeysetCursor([]driver.Value{driver.Int(7), driver.String("x"), driver.Time(ts)})
	if err != nil {
		t.Fatal(err)
	}
	vals, err := decodeKeysetCursor(cur, s)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := vals[0].AsInt(); n != 7 {
		t.Errorf("id: %s", vals[0])
	}
	if got, _ := vals[2].AsTime(); !got.Equal(ts) {
		t.Errorf("time: %s", vals[2])
	}
	if _, err := decodeKeysetCursor(cur, peopleSchema()); err == nil {
		t.Error("a cursor with the wrong number of keys must be refused")
	}
}
