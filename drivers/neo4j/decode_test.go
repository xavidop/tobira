package neo4j

import (
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j/dbtype"

	"github.com/xavidop/tobira/driver"
)

func TestDecode(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	node := dbtype.Node{ElementId: "4:abc:1", Labels: []string{"Person"}, Props: map[string]any{"name": "Ada", "age": int64(36)}}
	rel := dbtype.Relationship{ElementId: "5:abc:9", StartElementId: "4:abc:1", EndElementId: "4:abc:2", Type: "KNOWS", Props: map[string]any{"since": int64(2020)}}
	for _, tc := range []struct {
		in   any
		want string
		typ  driver.Type
	}{
		{nil, "NULL", driver.TypeNull},
		{true, "true", driver.TypeBool},
		{int64(7), "7", driver.TypeInt},
		{1.5, "1.5", driver.TypeFloat},
		{"x", "x", driver.TypeString},
		{ts, ts.Format(time.RFC3339Nano), driver.TypeTime},
		{dbtype.Date(ts), "2026-01-02T03:04:05Z", driver.TypeTime},
		{dbtype.Duration{Months: 1, Days: 2, Seconds: 3, Nanos: 4}, "P1M2DT3.000000004S", driver.TypeString},
		{dbtype.Point2D{X: 1, Y: 2, SpatialRefId: 4326}, `{"srid":4326,"x":1,"y":2}`, driver.TypeObject},
		{[]any{int64(1), "a"}, `[1,"a"]`, driver.TypeList},
		{map[string]any{"b": int64(1), "a": "x"}, `{"a":"x","b":1}`, driver.TypeObject},
		{node, `{"id":"4:abc:1","labels":["Person"],"properties":{"age":36,"name":"Ada"}}`, driver.TypeObject},
		{rel, `{"id":"5:abc:9","type":"KNOWS","start":"4:abc:1","end":"4:abc:2","properties":{"since":2020}}`, driver.TypeObject},
		{dbtype.Path{Nodes: []dbtype.Node{node}, Relationships: []dbtype.Relationship{rel}},
			`{"nodes":[{"id":"4:abc:1","labels":["Person"],"properties":{"age":36,"name":"Ada"}}],"relationships":[{"id":"5:abc:9","type":"KNOWS","start":"4:abc:1","end":"4:abc:2","properties":{"since":2020}}]}`, driver.TypeObject},
	} {
		got := decode(tc.in)
		if got.Type() != tc.typ {
			t.Errorf("%T: type %s, want %s", tc.in, got.Type(), tc.typ)
		}
		if s := got.String(); s != tc.want {
			t.Errorf("%T: %s, want %s", tc.in, s, tc.want)
		}
	}
}

func TestEncode(t *testing.T) {
	for _, tc := range []struct {
		in   driver.Value
		want any
	}{
		{driver.Null(), nil},
		{driver.Int(3), int64(3)},
		{driver.Decimal("12.50"), 12.5},
		{driver.JSON([]byte(`{"a":[1,"x"]}`)), map[string]any{"a": []any{1.0, "x"}}},
		{driver.List(driver.Int(1), driver.String("a")), []any{int64(1), "a"}},
		{driver.ObjectValue(driver.Member{Key: "k", Value: driver.Bool(true)}), map[string]any{"k": true}},
	} {
		got, err := encode(tc.in)
		if err != nil {
			t.Errorf("%s: %v", tc.in, err)
			continue
		}
		if g, w := driver.Value.String(decode(got)), driver.Value.String(decode(tc.want)); g != w {
			t.Errorf("%s: %s, want %s", tc.in, g, w)
		}
	}
	if _, err := encode(driver.Decimal("1e999")); err == nil {
		t.Error("a decimal beyond a float was accepted")
	}
	if _, err := encode(driver.JSON([]byte("{"))); err == nil {
		t.Error("invalid JSON was accepted")
	}
}

func TestTypeOfTypes(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want driver.Type
	}{
		{[]string{"Long"}, driver.TypeInt},
		{[]string{"Double"}, driver.TypeFloat},
		{[]string{"StringArray"}, driver.TypeList},
		{[]string{"DateTime", "Date"}, driver.TypeTime},
		{[]string{"Long", "String"}, driver.TypeUnknown},
		{nil, driver.TypeUnknown},
	} {
		if got := typeOfTypes(tc.in); got != tc.want {
			t.Errorf("%v: %s, want %s", tc.in, got, tc.want)
		}
	}
}
