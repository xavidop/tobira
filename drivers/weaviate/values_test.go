package weaviate

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/weaviate/weaviate/entities/models"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestIDValue(t *testing.T) {
	for _, tt := range []struct {
		uuid string
		want driver.Value
	}{
		{"00000000-0000-0000-0000-000000000001", driver.Int(1)},
		{"00000000-0000-0000-0000-00000000000a", driver.Int(10)},
		{"00000000-0000-0000-0000-000000000000", driver.Int(0)},
		{"00000000-0000-0000-7fff-ffffffffffff", driver.Int(math.MaxInt64)},
		{"00000000-0000-0000-8000-000000000000", driver.String("00000000-0000-0000-8000-000000000000")},
		{"00000000-0000-0001-0000-000000000001", driver.String("00000000-0000-0001-0000-000000000001")},
		{"5C56C793-69F3-4FBF-87E6-C4BF54C28C26", driver.String("5c56c793-69f3-4fbf-87e6-c4bf54c28c26")},
	} {
		if got := idValue(tt.uuid); !drivertest.EqualValue(tt.want, got) || got.Type() != tt.want.Type() {
			t.Errorf("idValue(%s) = %s (%s), want %s", tt.uuid, got, got.Type(), tt.want)
		}
	}
}

func TestToUUID(t *testing.T) {
	for _, tt := range []struct {
		in   driver.Value
		want string
	}{
		{driver.Int(1), "00000000-0000-0000-0000-000000000001"},
		{driver.Int(1000), "00000000-0000-0000-0000-0000000003e8"},
		{driver.Int(math.MaxInt64), "00000000-0000-0000-7fff-ffffffffffff"},
		{driver.String("5C56C793-69F3-4FBF-87E6-C4BF54C28C26"), "5c56c793-69f3-4fbf-87e6-c4bf54c28c26"},
		{driver.String("42"), "00000000-0000-0000-0000-00000000002a"},
		{driver.Decimal("7"), "00000000-0000-0000-0000-000000000007"},
	} {
		got, err := toUUID(tt.in, "insert")
		if err != nil || got != tt.want {
			t.Errorf("toUUID(%s) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
		if back := idValue(got); tt.in.Type() == driver.TypeInt && !drivertest.EqualValue(tt.in, back) {
			t.Errorf("idValue(toUUID(%s)) = %s", tt.in, back)
		}
	}
	for _, bad := range []driver.Value{driver.Int(-1), driver.String("x"), driver.Float(1.5), driver.Bool(true), driver.Null(), driver.String("5c56c793-69f3-4fbf-87e6-c4bf54c28c2")} {
		if got, err := toUUID(bad, "insert"); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("toUUID(%s) = %q, %v; want ErrSyntax", bad, got, err)
		}
	}
}

func TestTypeOf(t *testing.T) {
	for _, tt := range []struct {
		dt   string
		want driver.Type
	}{
		{"text", driver.TypeString}, {"string", driver.TypeString}, {"uuid", driver.TypeString},
		{"int", driver.TypeInt}, {"number", driver.TypeFloat}, {"boolean", driver.TypeBool},
		{"date", driver.TypeTime}, {"blob", driver.TypeBytes},
		{"geoCoordinates", driver.TypeObject}, {"phoneNumber", driver.TypeObject}, {"object", driver.TypeObject},
		{"text[]", driver.TypeList}, {"int[]", driver.TypeList}, {"object[]", driver.TypeList}, {"uuid[]", driver.TypeList},
		{"Person", driver.TypeUnknown},
	} {
		if got := typeOf([]string{tt.dt}); got != tt.want {
			t.Errorf("typeOf(%s) = %s, want %s", tt.dt, got, tt.want)
		}
	}
	if got := typeOf([]string{"Person", "Company"}); got != driver.TypeUnknown {
		t.Errorf("multi-class reference: %s", got)
	}
}

func decodeJSON(t *testing.T, s string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestDecodeProp(t *testing.T) {
	when := time.Date(2026, 1, 2, 3, 4, 5, 600, time.UTC)
	nested := []*models.NestedProperty{
		{Name: "name", DataType: []string{"text"}},
		{Name: "age", DataType: []string{"int"}},
		{Name: "tags", DataType: []string{"text[]"}},
	}
	for _, tt := range []struct {
		dt   string
		json string
		want driver.Value
	}{
		{"text", `"héllo"`, driver.String("héllo")},
		{"int", `9007199254740993`, driver.Int(9007199254740993)},
		{"number", `12.5`, driver.Float(12.5)},
		{"number", `3`, driver.Float(3)},
		{"boolean", `true`, driver.Bool(true)},
		{"date", `"2026-01-02T03:04:05.0000006Z"`, driver.Time(when)},
		{"uuid", `"5c56c793-69f3-4fbf-87e6-c4bf54c28c26"`, driver.String("5c56c793-69f3-4fbf-87e6-c4bf54c28c26")},
		{"blob", `"aGk="`, driver.Bytes([]byte("hi"))},
		{"text[]", `["a","b"]`, driver.List(driver.String("a"), driver.String("b"))},
		{"text[]", `[]`, driver.List()},
		{"int[]", `[1,2]`, driver.List(driver.Int(1), driver.Int(2))},
		{"date[]", `["2026-01-02T03:04:05.0000006Z"]`, driver.List(driver.Time(when))},
		{"text", `null`, driver.Null()},
		{"geoCoordinates", `{"longitude":2.5,"latitude":1}`, driver.ObjectValue(driver.Member{Key: "latitude", Value: driver.Float(1)}, driver.Member{Key: "longitude", Value: driver.Float(2.5)})},
	} {
		got := decodeProp([]string{tt.dt}, nil, decodeJSON(t, tt.json))
		if !drivertest.EqualValue(tt.want, got) || got.Type() != tt.want.Type() {
			t.Errorf("decodeProp(%s, %s) = %s (%s), want %s (%s)", tt.dt, tt.json, got, got.Type(), tt.want, tt.want.Type())
		}
	}
	got := decodeProp([]string{"object"}, nested, decodeJSON(t, `{"tags":["x"],"name":"n","age":3}`))
	want := driver.ObjectValue(
		driver.Member{Key: "name", Value: driver.String("n")},
		driver.Member{Key: "age", Value: driver.Int(3)},
		driver.Member{Key: "tags", Value: driver.List(driver.String("x"))},
	)
	if !drivertest.EqualValue(want, got) {
		t.Errorf("nested object = %s, want %s", got, want)
	}
	// A value that does not fit the declared type is kept, not lost.
	if got := decodeProp([]string{"int"}, nil, decodeJSON(t, `"oops"`)); got.Type() != driver.TypeString {
		t.Errorf("mismatched value = %s (%s)", got, got.Type())
	}
}

func TestEncodeProp(t *testing.T) {
	when := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		dt   string
		in   driver.Value
		want string
	}{
		{"text", driver.String("x"), `"x"`},
		{"int", driver.Int(33), `33`},
		{"int", driver.Float(4), `4`},
		{"int", driver.Decimal("9007199254740993"), `9007199254740993`},
		{"number", driver.Decimal("1.50"), `1.50`},
		{"number", driver.Int(2), `2`},
		{"number", driver.Float(0.25), `0.25`},
		{"boolean", driver.Bool(true), `true`},
		{"date", driver.Time(when), `"2026-06-01T12:00:00Z"`},
		{"date", driver.String("2026-06-01T12:00:00Z"), `"2026-06-01T12:00:00Z"`},
		{"uuid", driver.String("5c56c793-69f3-4fbf-87e6-c4bf54c28c26"), `"5c56c793-69f3-4fbf-87e6-c4bf54c28c26"`},
		{"text[]", driver.JSON([]byte(`[]`)), `[]`},
		{"text[]", driver.JSON([]byte(`["a","b"]`)), `["a","b"]`},
		{"text[]", driver.List(driver.String("a")), `["a"]`},
		{"int[]", driver.List(driver.Int(1), driver.Float(2)), `[1,2]`},
		{"blob", driver.Bytes([]byte("hi")), `"aGk="`},
		{"geoCoordinates", driver.ObjectValue(driver.Member{Key: "latitude", Value: driver.Float(1)}, driver.Member{Key: "longitude", Value: driver.Float(2)}), `{"latitude":1,"longitude":2}`},
		{"object", driver.JSON([]byte(`{"a":1}`)), `{"a":1}`},
		{"text", driver.Null(), `null`},
	} {
		v, err := encodeProp(propInfo{name: "p", dataType: []string{tt.dt}}, tt.in, "insert")
		if err != nil {
			t.Fatalf("encodeProp(%s, %s): %v", tt.dt, tt.in, err)
		}
		b, _ := json.Marshal(v)
		if string(b) != tt.want {
			t.Errorf("encodeProp(%s, %s) = %s, want %s", tt.dt, tt.in, b, tt.want)
		}
	}
	for _, tt := range []struct {
		dt string
		in driver.Value
	}{
		{"text", driver.Int(1)},
		{"int", driver.Float(1.5)},
		{"int", driver.String("1")},
		{"number", driver.Float(math.NaN())},
		{"number", driver.Float(math.Inf(1))},
		{"boolean", driver.Int(1)},
		{"date", driver.String("yesterday")},
		{"text[]", driver.String("a")},
		{"text[]", driver.List(driver.Int(1))},
		{"text[]", driver.JSON([]byte(`{"a":1}`))},
		{"Person", driver.String("x")},
	} {
		if _, err := encodeProp(propInfo{name: "p", dataType: []string{tt.dt}}, tt.in, "insert"); err == nil {
			t.Errorf("encodeProp(%s, %s) succeeded, want an error", tt.dt, tt.in)
		}
	}
}

func TestDistanceFromWeaviate(t *testing.T) {
	if got := distanceOf("l2-squared", 4); got != 2 {
		t.Errorf("l2-squared 4 = %v, want 2", got)
	}
	if got := distanceOf("cosine", 0.25); got != 0.25 {
		t.Errorf("cosine = %v", got)
	}
	if got := distanceOf("dot", -6); got != -6 {
		t.Errorf("dot = %v", got)
	}
	if got := distanceOf("manhattan", 3); got != 3 {
		t.Errorf("manhattan = %v", got)
	}
	for d, m := range map[string]driver.VectorMetric{"l2-squared": driver.MetricL2, "cosine": driver.MetricCosine, "dot": driver.MetricInnerProduct, "hamming": driver.MetricDefault, "manhattan": driver.MetricDefault} {
		if got := metricOf(d); got != m {
			t.Errorf("metricOf(%s) = %q, want %q", d, got, m)
		}
	}
}
