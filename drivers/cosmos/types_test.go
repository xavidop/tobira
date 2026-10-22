package cosmos

import (
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestDecodeValue(t *testing.T) {
	v, err := decodeValue([]byte(`{"b":1,"a":2.5,"s":"x","n":null,"l":[1,"a",[true]],"o":{"z":true,"y":{}},
		"big":9223372036854775808,"neg":-3,"e":1e3,"f":1.0,"esc":"é\n"}`))
	if err != nil {
		t.Fatal(err)
	}
	obj, ok := v.AsObject()
	if !ok {
		t.Fatalf("decodeValue gave %s, want an object", v.Type())
	}
	want := driver.Object{
		{Key: "b", Value: driver.Int(1)},
		{Key: "a", Value: driver.Float(2.5)},
		{Key: "s", Value: driver.String("x")},
		{Key: "n", Value: driver.Null()},
		{Key: "l", Value: driver.List(driver.Int(1), driver.String("a"), driver.List(driver.Bool(true)))},
		{Key: "o", Value: driver.ObjectValue(driver.Member{Key: "z", Value: driver.Bool(true)}, driver.Member{Key: "y", Value: driver.ObjectValue()})},
		{Key: "big", Value: driver.Decimal("9223372036854775808")},
		{Key: "neg", Value: driver.Int(-3)},
		{Key: "e", Value: driver.Float(1000)},
		{Key: "f", Value: driver.Float(1)},
		{Key: "esc", Value: driver.String("é\n")},
	}
	if len(obj) != len(want) {
		t.Fatalf("got %d members, want %d: %v", len(obj), len(want), obj)
	}
	for i := range want {
		if obj[i].Key != want[i].Key || obj[i].Value.Type() != want[i].Value.Type() || !drivertest.EqualValue(want[i].Value, obj[i].Value) {
			t.Errorf("member %d: got %s=%s (%s), want %s=%s (%s)", i, obj[i].Key, obj[i].Value, obj[i].Value.Type(), want[i].Key, want[i].Value, want[i].Value.Type())
		}
	}
	for _, bad := range []string{`{"a":`, `{"a":1}x`, `{"a" 1}`, ``} {
		if _, err := decodeValue([]byte(bad)); err == nil {
			t.Errorf("decodeValue(%q): want an error", bad)
		}
	}
	if v, err := decodeValue([]byte(`12`)); err != nil || !drivertest.EqualValue(driver.Int(12), v) {
		t.Errorf("decodeValue(12) = %s, %v", v, err)
	}
}

func TestDecodeDepthLimit(t *testing.T) {
	deep := make([]byte, 0, 2*(maxDepth+2))
	for range maxDepth + 2 {
		deep = append(deep, '[')
	}
	for range maxDepth + 2 {
		deep = append(deep, ']')
	}
	if _, err := decodeValue(deep); err == nil {
		t.Error("decodeValue of a too deeply nested array: want an error")
	}
}

func TestEncodeValue(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 600, time.UTC)
	doc := driver.ObjectValue(
		driver.Member{Key: "id", Value: driver.String("1")},
		driver.Member{Key: "n", Value: driver.Int(7)},
		driver.Member{Key: "f", Value: driver.Float(1.5)},
		driver.Member{Key: "d", Value: driver.Decimal("12.50")},
		driver.Member{Key: "b", Value: driver.Bool(true)},
		driver.Member{Key: "t", Value: driver.Time(ts)},
		driver.Member{Key: "raw", Value: driver.Bytes([]byte{1, 2})},
		driver.Member{Key: "j", Value: driver.JSON([]byte(`["a"]`))},
		driver.Member{Key: "v", Value: driver.Vector([]float32{0.5, 1})},
		driver.Member{Key: "z", Value: driver.Null()},
	)
	got, err := encodeValue(doc)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"1","n":7,"f":1.5,"d":12.50,"b":true,"t":"2026-01-02T03:04:05.0000006Z","raw":"AQI=","j":["a"],"v":[0.5,1],"z":null}`
	if string(got) != want {
		t.Errorf("encodeValue:\n got %s\nwant %s", got, want)
	}
	for _, bad := range []driver.Value{
		driver.Unknown("geo", nil, "x"),
		driver.Float(math.NaN()),
		driver.Float(math.Inf(1)),
		driver.Decimal("NaN"),
		driver.List(driver.Unknown("x", nil, "x")),
		driver.JSON([]byte(`{bad`)),
	} {
		if _, err := encodeValue(bad); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("encodeValue(%s): want ErrUnsupported, got %v", bad, err)
		}
	}
}

func TestPartitionKeyHelpers(t *testing.T) {
	for _, tc := range []struct{ path, field string }{
		{"/id", "id"}, {"/tenant", "tenant"}, {"/a/b", "a.b"}, {`/"odd name"`, "odd name"},
	} {
		if got := pkFieldName(tc.path); got != tc.field {
			t.Errorf("pkFieldName(%q) = %q, want %q", tc.path, got, tc.field)
		}
	}
	doc := driver.Object{
		{Key: "id", Value: driver.String("1")},
		{Key: "a", Value: driver.ObjectValue(driver.Member{Key: "b", Value: driver.Int(4)})},
	}
	if v, ok := pkValueAt(doc, "/a/b"); !ok || !drivertest.EqualValue(driver.Int(4), v) {
		t.Errorf("pkValueAt(/a/b) = %s, %v", v, ok)
	}
	if _, ok := pkValueAt(doc, "/missing"); ok {
		t.Error("pkValueAt(/missing): want not found")
	}
	pk, err := partitionKeyOf([]driver.Value{driver.String("acme"), driver.Int(2), driver.Bool(true), driver.Null(), driver.Float(1.5), driver.Decimal("3")})
	if err != nil {
		t.Fatal(err)
	}
	want := azcosmos.NewPartitionKeyString("acme").AppendNumber(2).AppendBool(true).AppendNull().AppendNumber(1.5).AppendNumber(3)
	if !reflect.DeepEqual(pk, want) {
		t.Errorf("partitionKeyOf = %#v, want %#v", pk, want)
	}
	if _, err := partitionKeyOf([]driver.Value{driver.List()}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("a list partition key value: want ErrUnsupported, got %v", err)
	}
	jpk, err := partitionKeyFromJSON([]any{"acme", 2.0, true, nil})
	if err != nil || !reflect.DeepEqual(jpk, azcosmos.NewPartitionKeyString("acme").AppendNumber(2).AppendBool(true).AppendNull()) {
		t.Errorf("partitionKeyFromJSON = %#v, %v", jpk, err)
	}
}
