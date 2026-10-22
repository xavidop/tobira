package cassandra

import (
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	gocql "github.com/apache/cassandra-gocql-driver/v2"
	"gopkg.in/inf.v0"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestDecodeScalars(t *testing.T) {
	ts := time.Date(2026, 2, 1, 10, 30, 0, 123e6, time.UTC)
	uuid, _ := gocql.ParseUUID("123e4567-e89b-12d3-a456-426614174000")
	dec, _ := new(inf.Dec).SetString("12.50")
	big1, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	for _, tc := range []struct {
		typ  string
		in   any
		want driver.Value
	}{
		{"int", 7, driver.Int(7)},
		{"bigint", int64(-9), driver.Int(-9)},
		{"counter", int64(42), driver.Int(42)},
		{"smallint", int16(300), driver.Int(300)},
		{"tinyint", int8(-5), driver.Int(-5)},
		{"text", "héllo", driver.String("héllo")},
		{"ascii", "abc", driver.String("abc")},
		{"boolean", true, driver.Bool(true)},
		{"float", float32(0.1), driver.Float(0.1)},
		{"double", 2.25, driver.Float(2.25)},
		{"decimal", *dec, driver.Decimal("12.50")},
		{"varint", *big1, driver.Decimal("123456789012345678901234567890")},
		{"timestamp", ts, driver.Time(ts)},
		{"date", time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), driver.Time(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))},
		{"time", 13*time.Hour + 45*time.Minute + 6*time.Second + 123*time.Millisecond, driver.String("13:45:06.123")},
		{"time", 5 * time.Hour, driver.String("05:00:00")},
		{"uuid", uuid, driver.String("123e4567-e89b-12d3-a456-426614174000")},
		{"timeuuid", uuid, driver.String("123e4567-e89b-12d3-a456-426614174000")},
		{"inet", net.ParseIP("10.0.0.1"), driver.String("10.0.0.1")},
		{"duration", gocql.Duration{Months: 14, Days: 2, Nanoseconds: int64(3*time.Hour + 4*time.Minute + 5*time.Second + 6*time.Millisecond)}, driver.String("1y2mo2d3h4m5s6ms")},
		{"duration", gocql.Duration{Nanoseconds: -int64(90 * time.Second)}, driver.String("-1m30s")},
		{"duration", gocql.Duration{}, driver.String("0s")},
		{"blob", []byte{1, 2, 3}, driver.Bytes([]byte{1, 2, 3})},
	} {
		info := ti(t, tc.typ)
		got, err := decodeValue(info, mustMarshal(t, info, tc.in))
		if err != nil {
			t.Errorf("decode %s %v: %v", tc.typ, tc.in, err)
			continue
		}
		if got.Type() != tc.want.Type() || !drivertest.EqualValue(tc.want, got) {
			t.Errorf("decode %s %v = %s (%s), want %s (%s)", tc.typ, tc.in, got, got.Type(), tc.want, tc.want.Type())
		}
	}
	got, _ := decodeValue(ti(t, "timestamp"), mustMarshal(t, ti(t, "timestamp"), ts))
	if gt, _ := got.AsTime(); !gt.Equal(ts) || gt.Location() != time.UTC {
		t.Errorf("timestamp = %v, want %v in UTC (milliseconds kept)", gt, ts)
	}
}

func TestDecodeNull(t *testing.T) {
	for _, typ := range []string{"int", "text", "list<int>", "map<text, text>", "decimal"} {
		got, err := decodeValue(ti(t, typ), nil)
		if err != nil || !got.IsNull() {
			t.Errorf("decode %s nil = %s, %v; want NULL", typ, got, err)
		}
	}
}

func TestDecodeCollections(t *testing.T) {
	str := driver.String
	for _, tc := range []struct {
		info gocql.TypeInfo
		in   any
		want driver.Value
	}{
		{ti(t, "list<int>"), []int{1, 2}, driver.List(driver.Int(1), driver.Int(2))},
		{ti(t, "frozen<list<text>>"), []string{}, driver.List()},
		{ti(t, "set<text>"), []string{"a", "b"}, driver.List(str("a"), str("b"))},
		{ti(t, "map<text, int>"), map[string]int{"x": 1}, driver.ObjectValue(driver.Member{Key: "x", Value: driver.Int(1)})},
		{ti(t, "map<int, text>"), map[int]string{7: "seven"}, driver.ObjectValue(driver.Member{Key: "7", Value: str("seven")})},
		{ti(t, "list<frozen<map<text, int>>>"), []map[string]int{{"a": 1}}, driver.List(driver.ObjectValue(driver.Member{Key: "a", Value: driver.Int(1)}))},
		{gocql.TupleTypeInfo{Elems: []gocql.TypeInfo{ti(t, "int"), ti(t, "text")}}, []any{3, nil}, driver.List(driver.Int(3), driver.Null())},
		{
			gocql.UDTTypeInfo{Keyspace: "ks", Name: "address", Elements: []gocql.UDTField{{Name: "street", Type: ti(t, "text")}, {Name: "zip", Type: ti(t, "int")}}},
			map[string]any{"street": "Main", "zip": 1000},
			driver.ObjectValue(driver.Member{Key: "street", Value: str("Main")}, driver.Member{Key: "zip", Value: driver.Int(1000)}),
		},
		{gocql.VectorType{SubType: ti(t, "float"), Dimensions: 3}, []float32{1, 2.5, -3}, driver.Vector([]float32{1, 2.5, -3})},
	} {
		got, err := decodeValue(tc.info, mustMarshal(t, tc.info, tc.in))
		if err != nil {
			t.Errorf("decode %s: %v", nativeName(tc.info), err)
			continue
		}
		if got.Type() != tc.want.Type() || !drivertest.EqualValue(tc.want, got) {
			t.Errorf("decode %s = %s (%s), want %s", nativeName(tc.info), got, got.Type(), tc.want)
		}
	}
}

func TestDecodeShortUDT(t *testing.T) {
	short := gocql.UDTTypeInfo{Name: "u", Elements: []gocql.UDTField{{Name: "a", Type: ti(t, "int")}}}
	long := gocql.UDTTypeInfo{Name: "u", Elements: []gocql.UDTField{{Name: "a", Type: ti(t, "int")}, {Name: "b", Type: ti(t, "text")}}}
	got, err := decodeValue(long, mustMarshal(t, short, map[string]any{"a": 1}))
	if err != nil {
		t.Fatal(err)
	}
	want := driver.ObjectValue(driver.Member{Key: "a", Value: driver.Int(1)}, driver.Member{Key: "b", Value: driver.Null()})
	if !drivertest.EqualValue(want, got) {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestDecodeMalformed(t *testing.T) {
	for _, tc := range []struct {
		typ  string
		data []byte
	}{
		{"int", []byte{1, 2}},
		{"bigint", []byte{1}},
		{"list<int>", []byte{0, 0, 0, 5}},
		{"list<int>", []byte{0, 0, 0, 1, 0, 0, 0, 9, 1}},
		{"map<text, int>", []byte{0, 0, 0, 1, 0, 0, 0, 1, 'a'}},
		{"list<int>", []byte{0xff, 0xff, 0xff, 0xff}},
	} {
		if _, err := decodeValue(ti(t, tc.typ), tc.data); err == nil {
			t.Errorf("decode %s %v: accepted malformed data", tc.typ, tc.data)
		}
	}
}

func TestDecodeCaps(t *testing.T) {
	old := maxCollectionElems
	maxCollectionElems = 3
	t.Cleanup(func() { maxCollectionElems = old })
	info := ti(t, "list<int>")
	got, err := decodeValue(info, mustMarshal(t, info, []int{1, 2, 3, 4}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Type() != driver.TypeUnknown || !strings.Contains(got.String(), "4 elements") {
		t.Errorf("an oversized collection = %s (%s), want an Unknown placeholder naming its size", got, got.Type())
	}
	got, err = decodeValue(info, mustMarshal(t, info, []int{1, 2, 3}))
	if err != nil || got.Type() != driver.TypeList {
		t.Errorf("a collection at the cap = %s, %v; want a List", got, err)
	}
}

func TestDecodeDepthCap(t *testing.T) {
	name := "int"
	for range maxDepth + 2 {
		name = "frozen<list<" + name + ">>"
	}
	info := ti(t, name)
	var v any = 1
	for range maxDepth + 2 {
		v = []any{v}
	}
	got, err := decodeValue(info, mustMarshal(t, info, v))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.String(), "too deep") {
		t.Errorf("a deeply nested value = %s, want a placeholder below the depth cap", got)
	}
}

// An empty, non-NULL value of a fixed-width type (legal in CQL, as
// blobAsInt(0x), and common in data written over Thrift) reads as NULL
// instead of failing the whole result; an empty text or blob stays empty.
func TestDecodeEmptyValue(t *testing.T) {
	for _, typ := range []string{"int", "bigint", "boolean", "double", "float", "timestamp", "date", "time", "uuid", "inet", "decimal", "varint", "duration", "list<int>", "map<text, int>"} {
		got, err := decodeValue(ti(t, typ), []byte{})
		if err != nil || !got.IsNull() {
			t.Errorf("decode empty %s = %s, %v; want NULL", typ, got, err)
		}
	}
	if got, err := decodeValue(ti(t, "text"), []byte{}); err != nil || got.Type() != driver.TypeString {
		t.Errorf("empty text = %s (%s), %v; want an empty String", got, got.Type(), err)
	}
	if got, err := decodeValue(ti(t, "blob"), []byte{}); err != nil || got.Type() != driver.TypeBytes {
		t.Errorf("empty blob = %s (%s), %v; want empty Bytes", got, got.Type(), err)
	}
}
