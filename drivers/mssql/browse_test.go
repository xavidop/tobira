package mssql

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func peopleSchema() *driver.CollectionSchema {
	return &driver.CollectionSchema{
		Ref: driver.CollectionRef{Name: "p"},
		Fields: []driver.Field{
			{Name: "id", Type: driver.TypeInt}, {Name: "ts", Type: driver.TypeTime}, {Name: "name", Type: driver.TypeString},
			{Name: "Name", Type: driver.TypeString}, {Name: "flag", Type: driver.TypeBool}, {Name: "amount", Type: driver.TypeDecimal},
		},
		PrimaryKey: []string{"id", "ts"},
	}
}

func TestKeysetCursorRoundTrip(t *testing.T) {
	s := peopleSchema()
	key := []driver.Value{driver.Int(7), driver.Time(time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC))}
	cur, err := encodeKeysetCursor(key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeKeysetCursor(cur, s)
	if err != nil {
		t.Fatal(err)
	}
	for i := range key {
		if !drivertest.EqualValue(key[i], got[i]) {
			t.Errorf("key %d: got %s, want %s", i, got[i], key[i])
		}
	}
	for _, bad := range []string{"x", "k:!!", "k:" + strings.Repeat("A", maxCursorLen)} {
		if _, err := decodeKeysetCursor(bad, s); err == nil {
			t.Errorf("decodeKeysetCursor(%q) succeeded", bad)
		}
	}
}

func TestKeysetWhereExpandsCompositeKeys(t *testing.T) {
	s := peopleSchema()
	cur, _ := encodeKeysetCursor([]driver.Value{driver.Int(7), driver.Time(time.Unix(0, 0).UTC())})
	b := &browseQuery{op: "browse", schema: s}
	if err := b.keysetWhere(cur); err != nil {
		t.Fatal(err)
	}
	want := "(([id] > @p1) OR ([id] = @p1 AND [ts] > @p2))"
	if len(b.where) != 1 || b.where[0] != want || len(b.params) != 2 {
		t.Errorf("where %q params %d, want %q", b.where, len(b.params), want)
	}
}

func TestBrowseConditions(t *testing.T) {
	b := &browseQuery{op: "browse", schema: peopleSchema()}
	for _, c := range []driver.Condition{
		{Field: "id", Op: driver.OpEq, Value: driver.Int(1)},
		{Field: "flag", Op: driver.OpNe, Value: driver.Null()},
		{Field: "id", Op: driver.OpContains, Value: driver.String("5%_[!")},
		{Field: "id", Op: driver.OpIn, Value: driver.List()},
		{Field: "id", Op: driver.OpIn, Value: driver.List(driver.Int(1), driver.Int(2))},
	} {
		col, err := b.field(c.Field)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.addCondition(col, c); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"[id] = @p1", "[flag] IS NOT NULL", "CAST([id] AS nvarchar(max)) LIKE N'%' + @p2 + N'%' ESCAPE '!'", "1 = 0", "[id] IN (@p3, @p4)"}
	if strings.Join(b.where, "|") != strings.Join(want, "|") {
		t.Errorf("where = %q\nwant    %q", b.where, want)
	}
	if s, _ := b.params[1].AsText(); s != "5!%!_![!!" {
		t.Errorf("LIKE pattern = %q", s)
	}
	if _, err := b.field("NAME"); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("ambiguous field: %v", err)
	}
	if _, err := b.field("name"); err != nil {
		t.Errorf("exact field: %v", err)
	}
	if err := b.addCondition("[id]", driver.Condition{Op: driver.OpEq, Value: driver.List(driver.Int(1))}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("list value: %v", err)
	}
}

func TestOffsetCursor(t *testing.T) {
	if n, err := parseOffsetCursor("o:40"); err != nil || n != 40 {
		t.Errorf("o:40 = %d, %v", n, err)
	}
	for _, bad := range []string{"40", "o:-1", "o:x", "k:abc"} {
		if _, err := parseOffsetCursor(bad); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("parseOffsetCursor(%q) = %v", bad, err)
		}
	}
}
