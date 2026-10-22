package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestParseCell(t *testing.T) {
	f := func(typ driver.Type, native string) fieldInfo {
		return fieldInfo{Name: "c", Type: typ, Native: native}
	}
	ok := []struct {
		f    fieldInfo
		in   string
		want driver.Value
	}{
		{f(driver.TypeInt, ""), " 42 ", driver.Int(42)},
		{f(driver.TypeInt, ""), "-7", driver.Int(-7)},
		{f(driver.TypeFloat, ""), "2.5e3", driver.Float(2500)},
		{f(driver.TypeDecimal, ""), "12.50", driver.Decimal("12.50")},
		{f(driver.TypeDecimal, ""), "-.5", driver.Decimal("-.5")},
		{f(driver.TypeBool, ""), "YES", driver.Bool(true)},
		{f(driver.TypeBool, ""), "f", driver.Bool(false)},
		{f(driver.TypeBool, ""), "1", driver.Bool(true)},
		{f(driver.TypeBool, ""), "No", driver.Bool(false)},
		{f(driver.TypeTime, ""), "2026-10-20T08:30:00Z", driver.Time(time.Date(2026, 10, 20, 8, 30, 0, 0, time.UTC))},
		{f(driver.TypeTime, ""), "2026-10-20 08:30:00.25", driver.Time(time.Date(2026, 10, 20, 8, 30, 0, 250e6, time.UTC))},
		{f(driver.TypeTime, ""), "2026-10-20", driver.Time(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))},
		{f(driver.TypeString, "uuid"), "0f8fad5b-d9cb-469f-a165-70867728950e", driver.String("0f8fad5b-d9cb-469f-a165-70867728950e")},
		{f(driver.TypeBytes, ""), `\xDEADbeef`, driver.Bytes([]byte{0xde, 0xad, 0xbe, 0xef})},
		{f(driver.TypeString, ""), "  keep\tme  ", driver.String("  keep\tme  ")},
		{f(driver.TypeUnknown, "money"), "$1.00", driver.String("$1.00")},
	}
	for _, tt := range ok {
		got, err := parseCell(tt.in, tt.f)
		if err != nil {
			t.Errorf("parseCell(%q, %s): %v", tt.in, tt.f.Type, err)
			continue
		}
		if !sameValue(got, tt.want) {
			t.Errorf("parseCell(%q, %s) = %#v, want %#v", tt.in, tt.f.Type, got, tt.want)
		}
	}
	bad := []struct {
		f   fieldInfo
		in  string
		err string
	}{
		{f(driver.TypeInt, ""), "4.5", "not a whole number"},
		{f(driver.TypeInt, ""), "", "not a whole number"},
		{f(driver.TypeFloat, ""), "x", "not a number"},
		{f(driver.TypeDecimal, ""), "1,5", "not a number"},
		{f(driver.TypeDecimal, ""), "NaN", "not a number"},
		{f(driver.TypeBool, ""), "maybe", "not a boolean"},
		{f(driver.TypeTime, ""), "20/10/2026", "not a time"},
		{f(driver.TypeString, "UUID"), "0f8fad5bd9cb469fa16570867728950e", "not a canonical UUID"},
		{f(driver.TypeBytes, ""), "dead", `not \x hex`},
		{f(driver.TypeBytes, ""), `\xabc`, `not \x hex`},
	}
	for _, tt := range bad {
		_, err := parseCell(tt.in, tt.f)
		if err == nil || !strings.Contains(err.Error(), tt.err) {
			t.Errorf("parseCell(%q, %s) err = %v, want %q", tt.in, tt.f.Type, err, tt.err)
		}
	}
}

func TestParseJSONCell(t *testing.T) {
	v, err := parseCell("{\n  \"b\": 1,\n  \"a\": [true, 2.5, \"x\", null]\n}", fieldInfo{Name: "o", Type: driver.TypeObject})
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String(); got != `{"b":1,"a":[true,2.5,"x",null]}` {
		t.Fatalf("object = %s (keys must keep their order)", got)
	}
	if v, err := parseCell(`[1, 2]`, fieldInfo{Type: driver.TypeList}); err != nil || v.Type() != driver.TypeList {
		t.Fatalf("list = %v, %v", v, err)
	}
	if v, err := parseCell(` {"k": [1]} `, fieldInfo{Type: driver.TypeJSON}); err != nil || v.String() != `{"k":[1]}` {
		t.Fatalf("json = %v, %v", v, err)
	}
	for _, tt := range []struct {
		typ driver.Type
		in  string
		err string
	}{
		{driver.TypeJSON, `{"a": }`, "not valid JSON"},
		{driver.TypeObject, `[1]`, "not a JSON object"},
		{driver.TypeList, `{}`, "not a JSON array"},
		{driver.TypeObject, `{"a":1} {}`, "not valid JSON"},
	} {
		if _, err := parseCell(tt.in, fieldInfo{Name: "j", Type: tt.typ}); err == nil || !strings.Contains(err.Error(), tt.err) {
			t.Errorf("parseCell(%q, %s) err = %v, want %q", tt.in, tt.typ, err, tt.err)
		}
	}
}

func TestEditText(t *testing.T) {
	long := make([]byte, 100)
	for _, tt := range []struct {
		v    driver.Value
		want string
	}{
		{driver.Null(), ""},
		{driver.Int(3), "3"},
		{driver.Bytes(long), `\x` + strings.Repeat("00", 100)}, // never cut like the grid's text
		{driver.Time(time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)), "2026-01-02T03:04:05.000000006Z"},
		{driver.JSON([]byte(`{"a":[1,2]}`)), "{\n  \"a\": [\n    1,\n    2\n  ]\n}"},
		{driver.String("a\nb"), "a\nb"},
	} {
		if got := editText(tt.v); got != tt.want {
			t.Errorf("editText(%v) = %q, want %q", tt.v, got, tt.want)
		}
	}
}

var peopleKey = []string{"id"}

func keyOf(id int64) driver.Object { return driver.Object{{Key: "id", Value: driver.Int(id)}} }

func TestPendingSetCountsAndOrder(t *testing.T) {
	p := newPendingSet(peopleKey)
	p.setCell(keyOf(3), "name", driver.String("Ada"), driver.String("Grace"))
	p.setCell(keyOf(3), "age", driver.Int(30), driver.Int(31))
	ins := p.addInsert()
	p.setInsertCell(ins, "id", driver.Int(100))
	p.setInsertCell(ins, "name", driver.String("New"))
	p.toggleDelete(keyOf(9))
	if n := p.count(); n != 4 { // 2 cells + 1 insert + 1 delete
		t.Fatalf("count = %d, want 4", n)
	}
	// Setting a cell back to its original value removes it.
	p.setCell(keyOf(3), "age", driver.Int(30), driver.Int(30))
	if n := p.count(); n != 3 {
		t.Fatalf("count after reverting a cell = %d, want 3", n)
	}
	// A deleted row's edits are not counted; unmarking brings them back.
	p.toggleDelete(keyOf(3))
	if n := p.count(); n != 3 { // insert + 2 deletes
		t.Fatalf("count with row 3 deleted = %d, want 3", n)
	}
	p.toggleDelete(keyOf(3))
	ch := p.changes()
	var got []string
	for _, c := range ch {
		got = append(got, c.kind+" "+c.label)
	}
	want := []string{"update id=3", "insert +1", "delete id=9"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	if v := ch[0].values; len(v) != 1 || v[0].Key != "name" {
		t.Fatalf("update values = %v", v)
	}
	if v := ch[1].values; len(v) != 2 || v[0].Key != "id" || v[1].Key != "name" {
		t.Fatalf("insert values = %v", v)
	}
	// Reverting every cell of a row drops it.
	p.setCell(keyOf(3), "name", driver.String("Ada"), driver.String("Ada"))
	p.removeInsert(ins)
	p.toggleDelete(keyOf(9))
	if !p.empty() || p.count() != 0 {
		t.Fatalf("pending not empty: %d", p.count())
	}
}

func TestPendingMissingFields(t *testing.T) {
	sch := &driver.CollectionSchema{Fields: []driver.Field{
		{Name: "id", Type: driver.TypeInt, Generated: true},
		{Name: "name", Type: driver.TypeString},
		{Name: "email", Type: driver.TypeString},
		{Name: "age", Type: driver.TypeInt, Nullable: true},
		{Name: "kind", Type: driver.TypeString, Default: "'x'"},
		{Name: "seq", Type: driver.TypeInt, AutoIncrement: true},
	}}
	p := newPendingSet(peopleKey)
	a := p.addInsert()
	p.setInsertCell(a, "name", driver.String("n"))
	p.setInsertCell(a, "email", driver.String("e"))
	b := p.addInsert()
	p.setInsertCell(b, "age", driver.Int(3))
	if msg := p.missing(sch); msg != "row +2 needs: name, email" {
		t.Fatalf("missing = %q", msg)
	}
	p.setInsertCell(b, "name", driver.Null()) // an explicit NULL is still a value the user chose
	p.setInsertCell(b, "email", driver.String("e"))
	if msg := p.missing(sch); msg != "" {
		t.Fatalf("missing = %q", msg)
	}
}

func TestPendingKeyLabels(t *testing.T) {
	k := driver.Object{{Key: "a", Value: driver.Int(1)}, {Key: "b", Value: driver.String("x\x1b[2J")}}
	if got := keyLabel(k); got != `a=1,b=x\x1b[2J` {
		t.Fatalf("keyLabel = %q", got)
	}
	// Int(1) and String("1") are different keys.
	if keyID(driver.Object{{Key: "a", Value: driver.Int(1)}}) == keyID(driver.Object{{Key: "a", Value: driver.String("1")}}) {
		t.Fatal("keys of different types collide")
	}
}

func TestPendingMarks(t *testing.T) {
	cols := []driver.Column{{Name: "id"}, {Name: "name"}, {Name: "age"}}
	p := newPendingSet(peopleKey)
	p.setCell(keyOf(1), "name", driver.String("a"), driver.String("b"))
	p.toggleDelete(keyOf(2))
	ins := p.addInsert()
	p.setInsertCell(ins, "age", driver.Int(5))
	mk := pendingMarks{p}
	m := mk.Row(cols, driver.Row{driver.Int(1), driver.String("a"), driver.Int(9)})
	if m.Values[1].String() != "b" || !m.Changed[1] || m.Changed[0] || m.Deleted {
		t.Fatalf("mark of row 1 = %+v", m)
	}
	if m := mk.Row(cols, driver.Row{driver.Int(2), driver.String("a"), driver.Int(9)}); !m.Deleted {
		t.Fatalf("mark of row 2 = %+v", m)
	}
	if m := mk.Row(cols, driver.Row{driver.Int(3), driver.String("a"), driver.Int(9)}); m.Values != nil || m.Deleted {
		t.Fatalf("mark of an untouched row = %+v", m)
	}
	pins := p.pinned()
	if len(pins) != 1 || len(pins[0].Values) != 1 || pins[0].Values[0].Key != "age" {
		t.Fatalf("pinned = %+v", pins)
	}
}

func TestParseJSONCellKeepsNumbersExact(t *testing.T) {
	in := `{"big": 123456789012345678901234, "d": 1.10, "e": 1e3, "i": 3, "neg": -0}`
	v, err := parseCell(in, fieldInfo{Name: "o", Type: driver.TypeObject})
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String(); got != `{"big":123456789012345678901234,"d":1.10,"e":1e3,"i":3,"neg":-0}` {
		t.Fatalf("object = %s", got)
	}
	l, err := parseCell(`[0.1, 9007199254740993]`, fieldInfo{Type: driver.TypeList})
	if err != nil || l.String() != `[0.1,9007199254740993]` {
		t.Fatalf("list = %v, %v", l, err)
	}
}
