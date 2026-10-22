package elasticsearch

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

const peopleMapping = `{"tobira_people":{"mappings":{"properties":{
 "active":{"type":"boolean"},
 "age":{"type":"integer"},
 "created_at":{"type":"date"},
 "email":{"type":"keyword"},
 "id":{"type":"integer"},
 "name":{"type":"text","fields":{"keyword":{"type":"keyword","ignore_above":256}}},
 "score":{"type":"scaled_float","scaling_factor":100},
 "tags":{"type":"object","enabled":false},
 "author":{"properties":{"name":{"type":"keyword"},"country":{"type":"keyword"}}},
 "vec":{"type":"dense_vector","dims":4,"index":true,"similarity":"l2_norm"},
 "blob":{"type":"binary"},
 "comments":{"type":"nested","properties":{"by":{"type":"keyword"}}},
 "labels":{"type":"flattened"},
 "loc":{"type":"geo_point"}
}}}}`

func TestParseMappings(t *testing.T) {
	m, indices, err := parseMappings([]byte(peopleMapping))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(indices, []string{"tobira_people"}) {
		t.Errorf("indices = %v", indices)
	}
	want := map[string]driver.Type{
		"active": driver.TypeBool, "age": driver.TypeInt, "created_at": driver.TypeTime,
		"email": driver.TypeString, "id": driver.TypeInt, "name": driver.TypeString,
		"score": driver.TypeDecimal, "tags": driver.TypeJSON, "author": driver.TypeObject,
		"vec": driver.TypeVector, "blob": driver.TypeBytes, "comments": driver.TypeList,
		"labels": driver.TypeJSON, "loc": driver.TypeJSON,
	}
	for name, typ := range want {
		f := m.lookup(name)
		if f == nil {
			t.Errorf("field %q missing", name)
			continue
		}
		if got := fieldType(f); got != typ {
			t.Errorf("field %q: type %s, want %s (native %q)", name, got, typ, nativeType(f))
		}
	}
	if f := m.lookup("author.name"); f == nil || f.typ != "keyword" {
		t.Errorf("author.name = %+v", f)
	}
	if p, ok := m.lookup("name").keywordPath("name"); !ok || p != "name.keyword" {
		t.Errorf("name keyword path = %q, %v", p, ok)
	}
	if _, ok := m.lookup("email").keywordPath("email"); !ok {
		t.Error("a keyword field is its own keyword path")
	}
	if got := nativeType(m.lookup("vec")); got != "dense_vector(4, l2_norm)" {
		t.Errorf("vec native type = %q", got)
	}
	if got := nativeType(m.lookup("tags")); got != "object (enabled: false)" {
		t.Errorf("tags native type = %q", got)
	}
}

func TestMergeMappingsAcrossIndices(t *testing.T) {
	m, indices, err := parseMappings([]byte(`{"b":{"mappings":{"properties":{"x":{"type":"long"},"y":{"type":"keyword"}}}},"a":{"mappings":{"properties":{"x":{"type":"keyword"}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(indices, []string{"a", "b"}) {
		t.Errorf("indices = %v, want sorted", indices)
	}
	if f := m.lookup("x"); f == nil || f.typ != "keyword" {
		t.Errorf("x = %+v, want the first index's (a) keyword", f)
	}
	if m.lookup("y") == nil {
		t.Error("y from the second index is missing")
	}
}

// People values convert exactly from their indexed JSON.
func TestConvertPeopleRow(t *testing.T) {
	m, _, err := parseMappings([]byte(peopleMapping))
	if err != nil {
		t.Fatal(err)
	}
	people := drivertest.PeopleDataset()
	for _, row := range people.Rows[:12] {
		for i, col := range people.Columns {
			raw, err := valueJSON(row[i])
			if err != nil {
				t.Fatalf("valueJSON(%s): %v", row[i], err)
			}
			got := convertValue(raw, m.lookup(col.Name))
			if !drivertest.EqualValue(row[i], got) || (row[i].Type() != got.Type() && !got.IsNull()) {
				t.Errorf("%s: %s (%s) round-tripped through %s to %s (%s)", col.Name, row[i], row[i].Type(), raw, got, got.Type())
			}
		}
	}
}

func TestConvertValue(t *testing.T) {
	m, _, err := parseMappings([]byte(peopleMapping))
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		field, raw string
		want       driver.Value
	}{
		{"id", `7`, driver.Int(7)},
		{"id", `"7"`, driver.Int(7)},
		{"id", `null`, driver.Null()},
		{"score", `12.50`, driver.Decimal("12.50")},
		{"score", `"3.25"`, driver.Decimal("3.25")},
		{"created_at", `"2026-01-02T03:04:05Z"`, driver.Time(ts)},
		{"created_at", `"2026-01-02T03:04:05.000+00:00"`, driver.Time(ts)},
		{"created_at", `"2026-01-02"`, driver.Time(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))},
		{"created_at", `1767323045000`, driver.Time(ts)},
		{"created_at", `"not a date"`, driver.String("not a date")},
		{"active", `true`, driver.Bool(true)},
		{"active", `"false"`, driver.Bool(false)},
		{"email", `"a@b"`, driver.String("a@b")},
		{"email", `["a","b"]`, driver.List(driver.String("a"), driver.String("b"))},
		{"email", `42`, driver.String("42")},
		{"tags", `["x",{"y":1}]`, driver.JSON([]byte(`["x",{"y":1}]`))},
		{"vec", `[1,0.5,0,-2]`, driver.Vector([]float32{1, 0.5, 0, -2})},
		{"blob", `"aGk="`, driver.Bytes([]byte("hi"))},
		{"author", `{"name":"Ada","country":"UK"}`, driver.ObjectValue(driver.Member{Key: "name", Value: driver.String("Ada")}, driver.Member{Key: "country", Value: driver.String("UK")})},
		{"comments", `{"by":"x"}`, driver.List(driver.ObjectValue(driver.Member{Key: "by", Value: driver.String("x")}))},
		{"loc", `{"lat":1,"lon":2}`, driver.JSON([]byte(`{"lat":1,"lon":2}`))},
	} {
		got := convertValue(json.RawMessage(tc.raw), m.lookup(tc.field))
		if got.Type() != tc.want.Type() || !drivertest.EqualValue(tc.want, got) {
			t.Errorf("convertValue(%s as %s) = %s (%s), want %s (%s)", tc.raw, tc.field, got, got.Type(), tc.want, tc.want.Type())
		}
	}
}

func TestGenericValue(t *testing.T) {
	for raw, want := range map[string]driver.Value{
		`1`:                    driver.Int(1),
		`1.5`:                  driver.Float(1.5),
		`"x"`:                  driver.String("x"),
		`true`:                 driver.Bool(true),
		`null`:                 driver.Null(),
		`[1,"a"]`:              driver.List(driver.Int(1), driver.String("a")),
		`{"a":{"b":2}}`:        driver.ObjectValue(driver.Member{Key: "a", Value: driver.ObjectValue(driver.Member{Key: "b", Value: driver.Int(2)})}),
		`18446744073709551615`: driver.Decimal("18446744073709551615"),
	} {
		got := convertValue(json.RawMessage(raw), nil)
		if got.Type() != want.Type() || !drivertest.EqualValue(want, got) {
			t.Errorf("generic %s = %s (%s), want %s (%s)", raw, got, got.Type(), want, want.Type())
		}
	}
}

func TestValueJSON(t *testing.T) {
	for _, tc := range []struct {
		v    driver.Value
		want string
	}{
		{driver.Null(), `null`},
		{driver.Int(-3), `-3`},
		{driver.Decimal("12.50"), `12.50`},
		{driver.Float(0.25), `0.25`},
		{driver.String(`a"b`), `"a\"b"`},
		{driver.Bool(false), `false`},
		{driver.Time(time.Date(2026, 6, 1, 12, 0, 0, 5, time.FixedZone("x", 3600))), `"2026-06-01T11:00:00.000000005Z"`},
		{driver.JSON([]byte(` [1, 2] `)), `[1,2]`},
		{driver.Bytes([]byte("hi")), `"aGk="`},
		{driver.Vector([]float32{1, 0.5}), `[1,0.5]`},
		{driver.List(driver.Int(1), driver.String("a")), `[1,"a"]`},
		{driver.ObjectValue(driver.Member{Key: "k", Value: driver.Int(1)}), `{"k":1}`},
	} {
		got, err := valueJSON(tc.v)
		if err != nil || string(got) != tc.want {
			t.Errorf("valueJSON(%s) = %s, %v; want %s", tc.v, got, err, tc.want)
		}
	}
	if _, err := valueJSON(driver.Decimal("NaN")); err == nil {
		t.Error("valueJSON accepted a decimal that is not a JSON number")
	}
}

func TestOrderedKeys(t *testing.T) {
	kvs, err := orderedObject([]byte(`{"id":1,"name":"x","a":{"z":1},"b":[1]}`))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, kv := range kvs {
		keys = append(keys, kv.key)
	}
	if !slices.Equal(keys, []string{"id", "name", "a", "b"}) {
		t.Errorf("keys = %v", keys)
	}
	if string(kvs[2].raw) != `{"z":1}` {
		t.Errorf("raw a = %s", kvs[2].raw)
	}
}
