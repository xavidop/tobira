package convert_test

import (
	"bytes"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/plugin/convert"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
)

// sameValue reports whether a and b are the same driver.Value exactly: same
// type, same bits for floats (NaN included), same bytes, same members in the
// same order, and times with the same instant and zone.
func sameValue(a, b driver.Value) bool {
	if a.Type() != b.Type() {
		return false
	}
	switch a.Type() {
	case driver.TypeNull:
		return true
	case driver.TypeBool:
		x, _ := a.AsBool()
		y, _ := b.AsBool()
		return x == y
	case driver.TypeInt:
		x, _ := a.AsInt()
		y, _ := b.AsInt()
		return x == y
	case driver.TypeFloat:
		x, _ := a.AsFloat()
		y, _ := b.AsFloat()
		return math.Float64bits(x) == math.Float64bits(y)
	case driver.TypeDecimal, driver.TypeString:
		x, _ := a.AsText()
		y, _ := b.AsText()
		return x == y
	case driver.TypeBytes:
		x, _ := a.AsBytes()
		y, _ := b.AsBytes()
		return bytes.Equal(x, y)
	case driver.TypeTime:
		x, _ := a.AsTime()
		y, _ := b.AsTime()
		return sameTime(x, y)
	case driver.TypeJSON:
		x, _ := a.AsJSON()
		y, _ := b.AsJSON()
		return bytes.Equal(x, y)
	case driver.TypeVector:
		x, _ := a.AsVector()
		y, _ := b.AsVector()
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if math.Float32bits(x[i]) != math.Float32bits(y[i]) {
				return false
			}
		}
		return true
	case driver.TypeList:
		x, _ := a.AsList()
		y, _ := b.AsList()
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if !sameValue(x[i], y[i]) {
				return false
			}
		}
		return true
	case driver.TypeObject:
		x, _ := a.AsObject()
		y, _ := b.AsObject()
		return sameObject(x, y)
	case driver.TypeUnknown:
		xn, xr, _ := a.AsUnknown()
		yn, yr, _ := b.AsUnknown()
		return xn == yn && bytes.Equal(xr, yr) && a.String() == b.String()
	}
	return false
}

func sameObject(x, y driver.Object) bool {
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i].Key != y[i].Key || !sameValue(x[i].Value, y[i].Value) {
			return false
		}
	}
	return true
}

func sameTime(x, y time.Time) bool {
	xn, xo := x.Zone()
	yn, yo := y.Zone()
	return x.Equal(y) && xn == yn && xo == yo &&
		x.Format(time.RFC3339Nano+" MST") == y.Format(time.RFC3339Nano+" MST")
}

// wire marshals p and unmarshals it into a fresh message, so a round trip
// also proves the value survives the protobuf encoding.
func wire[T proto.Message](t *testing.T, p T, fresh T) T {
	t.Helper()
	b, err := proto.Marshal(p)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	if err := proto.Unmarshal(b, fresh); err != nil {
		t.Fatalf("proto.Unmarshal: %v", err)
	}
	return fresh
}

func roundTrip(t *testing.T, v driver.Value) driver.Value {
	t.Helper()
	p, err := convert.ValueToProto(v)
	if err != nil {
		t.Fatalf("ValueToProto(%v): %v", v, err)
	}
	got, err := convert.ValueFromProto(wire(t, p, &pb.Value{}))
	if err != nil {
		t.Fatalf("ValueFromProto(%v): %v", v, err)
	}
	return got
}

func TestValueRoundTrip(t *testing.T) {
	madrid := time.FixedZone("CEST", 2*3600)
	nepal := time.FixedZone("+0545", 5*3600+45*60)
	cases := []struct {
		name string
		v    driver.Value
	}{
		{"null", driver.Null()},
		{"bool true", driver.Bool(true)},
		{"bool false", driver.Bool(false)},
		{"int zero", driver.Int(0)},
		{"int min", driver.Int(math.MinInt64)},
		{"int max", driver.Int(math.MaxInt64)},
		{"float", driver.Float(3.25)},
		{"float negative zero", driver.Float(math.Copysign(0, -1))},
		{"float NaN", driver.Float(math.NaN())},
		{"float +Inf", driver.Float(math.Inf(1))},
		{"float -Inf", driver.Float(math.Inf(-1))},
		{"float smallest", driver.Float(math.SmallestNonzeroFloat64)},
		{"decimal", driver.Decimal("12.50")},
		{"decimal uint64 overflow", driver.Decimal("18446744073709551615")},
		{"decimal long", driver.Decimal("-0.000000000000000000000000000000000001")},
		{"string", driver.String("héllo, 世界")},
		{"string empty", driver.String("")},
		{"string invalid utf8", driver.String("a\xff\xfeb")},
		{"string NUL", driver.String("a\x00b")},
		{"bytes", driver.Bytes([]byte{0, 1, 2, 0xff})},
		{"bytes empty", driver.Bytes(nil)},
		{"time utc nanos", driver.Time(time.Date(2026, 10, 20, 13, 14, 15, 123456789, time.UTC))},
		{"time fixed zone", driver.Time(time.Date(2026, 7, 1, 9, 0, 0, 1, madrid))},
		{"time odd offset", driver.Time(time.Date(2001, 2, 3, 4, 5, 6, 0, nepal))},
		{"time negative offset", driver.Time(time.Date(1999, 12, 31, 23, 59, 59, 999999999, time.FixedZone("EST", -5*3600)))},
		{"time unnamed zone", driver.Time(time.Date(2020, 1, 1, 0, 0, 0, 0, time.FixedZone("", 3600)))},
		{"time local", driver.Time(time.Date(2024, 3, 4, 5, 6, 7, 8, time.Local))},
		{"time zero", driver.Time(time.Time{})},
		{"time before epoch", driver.Time(time.Date(1900, 1, 1, 0, 0, 0, 500, time.UTC))},
		{"time far future", driver.Time(time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC))},
		{"json", driver.JSON([]byte(`{"b":1,"a":[true,null]}`))},
		{"json invalid kept raw", driver.JSON([]byte(`{not json`))},
		{"json empty", driver.JSON(nil)},
		{"vector", driver.Vector([]float32{1, -2.5, 0.125})},
		{"vector special", driver.Vector([]float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1)), math.SmallestNonzeroFloat32})},
		{"vector empty", driver.Vector(nil)},
		{"list empty", driver.List()},
		{"list mixed", driver.List(driver.Int(1), driver.Null(), driver.String("x"), driver.Float(math.NaN()))},
		{"object empty", driver.ObjectValue()},
		{"object keeps order and duplicates", driver.ObjectValue(
			driver.Member{Key: "z", Value: driver.Int(1)},
			driver.Member{Key: "a", Value: driver.Int(2)},
			driver.Member{Key: "z", Value: driver.Int(3)},
			driver.Member{Key: "", Value: driver.Null()},
		)},
		{"nested", driver.ObjectValue(
			driver.Member{Key: "list", Value: driver.List(
				driver.ObjectValue(driver.Member{Key: "v", Value: driver.Vector([]float32{1, 2})}),
				driver.List(driver.Decimal("1.0"), driver.Time(time.Date(2026, 1, 2, 3, 4, 5, 6, madrid))),
			)},
			driver.Member{Key: "bytes", Value: driver.Bytes([]byte("raw"))},
			driver.Member{Key: "unknown", Value: driver.Unknown("geometry", []byte{1, 2}, "POINT(1 2)")},
		)},
		{"unknown", driver.Unknown("tsvector", []byte("'a':1"), "'a':1")},
		{"unknown empty", driver.Unknown("", nil, "")},
		{"unknown invalid utf8", driver.Unknown("x\xff", []byte{0xff}, "\xfe")},
	}
	covered := map[driver.Type]bool{}
	for _, tc := range cases {
		covered[tc.v.Type()] = true
		t.Run(tc.name, func(t *testing.T) {
			got := roundTrip(t, tc.v)
			if !sameValue(tc.v, got) {
				t.Fatalf("round trip changed the value:\n want %v (%v)\n  got %v (%v)", tc.v, tc.v.Type(), got, got.Type())
			}
		})
	}
	for typ := driver.TypeNull; typ <= driver.TypeUnknown; typ++ {
		if !covered[typ] {
			t.Errorf("no round-trip case for %v", typ)
		}
	}
}

func TestTimeRoundTripKeepsZones(t *testing.T) {
	utc := roundTrip(t, driver.Time(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	if tm, _ := utc.AsTime(); tm.Location() != time.UTC {
		t.Errorf("UTC came back as %v", tm.Location())
	}
	local := roundTrip(t, driver.Time(time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local)))
	if tm, _ := local.AsTime(); tm.Location() != time.Local {
		t.Errorf("Local came back as %v", tm.Location())
	}
	zero := roundTrip(t, driver.Time(time.Time{}))
	if tm, _ := zero.AsTime(); !tm.IsZero() || tm != (time.Time{}) {
		t.Errorf("zero time came back as %#v", tm)
	}
}

func TestTimeLocalWithDifferentOffsetBecomesFixed(t *testing.T) {
	// A plugin whose Local differs from the host's: the wall clock and
	// offset are kept, under a fixed zone.
	when := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	_, localOff := when.In(time.Local).Zone()
	p := &pb.Time{Instant: nil, Zone: "Local", OffsetSeconds: int32(localOff + 3600)}
	p.Instant = convert.TimeToProto(when).GetInstant()
	got, err := convert.TimeFromProto(p)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(when) {
		t.Fatalf("instant changed: %v", got)
	}
	if _, off := got.Zone(); off != localOff+3600 {
		t.Fatalf("offset = %d, want %d", off, localOff+3600)
	}
}

func TestTimeFromProtoNil(t *testing.T) {
	got, err := convert.TimeFromProto(nil)
	if err != nil || !got.IsZero() {
		t.Fatalf("TimeFromProto(nil) = %v, %v; want the zero time", got, err)
	}
}

func TestValueFromProtoRejectsInvalid(t *testing.T) {
	deep := &pb.Value{Kind: &pb.Value_Null{Null: &pb.Null{}}}
	for range convert.MaxDepth + 1 {
		deep = &pb.Value{Kind: &pb.Value_ListValue{ListValue: &pb.List{Values: []*pb.Value{deep}}}}
	}
	long := strings.Repeat("k", convert.MaxNameBytes+1)
	cases := []struct {
		name string
		p    *pb.Value
		want error
	}{
		{"nil", nil, convert.ErrInvalid},
		{"no kind", &pb.Value{}, convert.ErrInvalid},
		{"nil list element", &pb.Value{Kind: &pb.Value_ListValue{ListValue: &pb.List{Values: []*pb.Value{nil}}}}, convert.ErrInvalid},
		{"nil member value", &pb.Value{Kind: &pb.Value_ObjectValue{ObjectValue: &pb.Object{Members: []*pb.Member{{Key: "a"}}}}}, convert.ErrInvalid},
		{"nil member", &pb.Value{Kind: &pb.Value_ObjectValue{ObjectValue: &pb.Object{Members: []*pb.Member{nil}}}}, convert.ErrInvalid},
		{"bad nanos", &pb.Value{Kind: &pb.Value_TimeValue{TimeValue: &pb.Time{Instant: timestamp(0, 1_000_000_000), Zone: "UTC"}}}, convert.ErrInvalid},
		{"negative nanos", &pb.Value{Kind: &pb.Value_TimeValue{TimeValue: &pb.Time{Instant: timestamp(0, -1), Zone: "UTC"}}}, convert.ErrInvalid},
		{"offset out of range", &pb.Value{Kind: &pb.Value_TimeValue{TimeValue: &pb.Time{Instant: timestamp(0, 0), Zone: "X", OffsetSeconds: 48 * 3600}}}, convert.ErrInvalid},
		{"too deep", deep, convert.ErrLimit},
		{"key too long", &pb.Value{Kind: &pb.Value_ObjectValue{ObjectValue: &pb.Object{Members: []*pb.Member{{Key: long, Value: nullProto()}}}}}, convert.ErrLimit},
		{"native type too long", &pb.Value{Kind: &pb.Value_UnknownValue{UnknownValue: &pb.Unknown{NativeType: long}}}, convert.ErrLimit},
		{"zone too long", &pb.Value{Kind: &pb.Value_TimeValue{TimeValue: &pb.Time{Instant: timestamp(0, 0), Zone: long}}}, convert.ErrLimit},
		{"too big", &pb.Value{Kind: &pb.Value_BytesValue{BytesValue: make([]byte, convert.MaxValueBytes)}}, convert.ErrLimit},
		{"too big nested", &pb.Value{Kind: &pb.Value_ListValue{ListValue: &pb.List{Values: []*pb.Value{
			{Kind: &pb.Value_StringValue{StringValue: strings.Repeat("a", convert.MaxValueBytes/2)}},
			{Kind: &pb.Value_StringValue{StringValue: strings.Repeat("b", convert.MaxValueBytes/2)}},
		}}}}, convert.ErrLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := convert.ValueFromProto(tc.p)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ValueFromProto = %v, %v; want %v", v, err, tc.want)
			}
		})
	}
}

func TestValueFromProtoAcceptsNilTimeAndContainers(t *testing.T) {
	// A missing sub-message decodes as its empty form, as protobuf readers
	// in other languages would send an empty list or vector.
	cases := []struct {
		p    *pb.Value
		want driver.Value
	}{
		{&pb.Value{Kind: &pb.Value_Null{}}, driver.Null()},
		{&pb.Value{Kind: &pb.Value_TimeValue{}}, driver.Time(time.Time{})},
		{&pb.Value{Kind: &pb.Value_VectorValue{}}, driver.Vector(nil)},
		{&pb.Value{Kind: &pb.Value_ListValue{}}, driver.List()},
		{&pb.Value{Kind: &pb.Value_ObjectValue{}}, driver.ObjectValue()},
		{&pb.Value{Kind: &pb.Value_UnknownValue{}}, driver.Unknown("", nil, "")},
	}
	for _, tc := range cases {
		got, err := convert.ValueFromProto(tc.p)
		if err != nil {
			t.Fatalf("ValueFromProto(%v): %v", tc.p, err)
		}
		if !sameValue(tc.want, got) {
			t.Errorf("ValueFromProto(%v) = %v, want %v", tc.p, got, tc.want)
		}
	}
}

func TestValueToProtoEnforcesLimits(t *testing.T) {
	deep := driver.Null()
	for range convert.MaxDepth + 1 {
		deep = driver.List(deep)
	}
	cases := []struct {
		name string
		v    driver.Value
	}{
		{"too big", driver.Bytes(make([]byte, convert.MaxValueBytes))},
		{"too deep", deep},
		{"key too long", driver.ObjectValue(driver.Member{Key: strings.Repeat("k", convert.MaxNameBytes+1)})},
		{"native type too long", driver.Unknown(strings.Repeat("t", convert.MaxNameBytes+1), nil, "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := convert.ValueToProto(tc.v); !errors.Is(err, convert.ErrLimit) {
				t.Fatalf("ValueToProto: err = %v, want ErrLimit", err)
			}
		})
	}
}

func TestValueAtLimitsIsAccepted(t *testing.T) {
	// Exactly at each cap is fine: the caps are maxima, not sentinels.
	depth := driver.Null()
	for range convert.MaxDepth {
		depth = driver.List(depth)
	}
	key := strings.Repeat("k", convert.MaxNameBytes)
	for _, v := range []driver.Value{
		depth,
		driver.ObjectValue(driver.Member{Key: key, Value: driver.Null()}),
		driver.Bytes(make([]byte, convert.MaxValueBytes-16)),
	} {
		if got := roundTrip(t, v); !sameValue(v, got) {
			t.Errorf("value at the limit did not round-trip: %v", v.Type())
		}
	}
}

func TestObjectRoundTrip(t *testing.T) {
	in := driver.Object{
		{Key: "id", Value: driver.Int(7)},
		{Key: "name", Value: driver.String("x")},
		{Key: "id", Value: driver.Decimal("8")},
	}
	p, err := convert.ObjectToProto(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := convert.ObjectFromProto(wire(t, p, &pb.Object{}))
	if err != nil {
		t.Fatal(err)
	}
	if !sameObject(in, out) {
		t.Fatalf("object changed: %v -> %v", in, out)
	}
	if got, err := convert.ObjectFromProto(nil); err != nil || got != nil {
		t.Fatalf("ObjectFromProto(nil) = %v, %v; want nil, nil", got, err)
	}
	if p, err := convert.ObjectToProto(nil); err != nil || len(p.GetMembers()) != 0 {
		t.Fatalf("ObjectToProto(nil) = %v, %v", p, err)
	}
}

func TestObjectFromProtoEnforcesTotalSize(t *testing.T) {
	big := strings.Repeat("a", convert.MaxValueBytes/2+1)
	p := &pb.Object{Members: []*pb.Member{
		{Key: "a", Value: &pb.Value{Kind: &pb.Value_StringValue{StringValue: big}}},
		{Key: "b", Value: &pb.Value{Kind: &pb.Value_StringValue{StringValue: big}}},
	}}
	// Each member's value is under the cap; the object as a whole is a
	// key set (Mutator), so the cap applies per member value.
	if _, err := convert.ObjectFromProto(p); err != nil {
		t.Fatalf("two values under the per-value cap: %v", err)
	}
	p.Members[0].Value = &pb.Value{Kind: &pb.Value_StringValue{StringValue: strings.Repeat("a", convert.MaxValueBytes)}}
	if _, err := convert.ObjectFromProto(p); !errors.Is(err, convert.ErrLimit) {
		t.Fatalf("a value over the cap: err = %v, want ErrLimit", err)
	}
}

func TestValuesRoundTrip(t *testing.T) {
	in := []driver.Value{driver.Int(1), driver.String("a"), driver.Null()}
	p, err := convert.ValuesToProto(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := convert.ValuesFromProto(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("len = %d", len(out))
	}
	for i := range in {
		if !sameValue(in[i], out[i]) {
			t.Errorf("value %d: %v -> %v", i, in[i], out[i])
		}
	}
}

func TestTimeFromProtoNilInstantIsZero(t *testing.T) {
	got, err := convert.TimeFromProto(&pb.Time{Zone: "UTC"})
	if err != nil || got != (time.Time{}) {
		t.Fatalf("TimeFromProto(no instant) = %#v, %v; want the zero time", got, err)
	}
}

func TestTimeFixedZoneNamedLocalBecomesLocal(t *testing.T) {
	// Documented inexactness: a fixed zone called "Local" whose offset matches
	// the host's local zone comes back as time.Local. Instant, offset and
	// abbreviation-free formatting are unchanged.
	when := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	_, off := when.In(time.Local).Zone()
	in := when.In(time.FixedZone("Local", off))
	got, err := convert.TimeFromProto(convert.TimeToProto(in))
	if err != nil {
		t.Fatal(err)
	}
	if got.Location() != time.Local || !got.Equal(in) {
		t.Fatalf("got %v in %v", got, got.Location())
	}
	if _, gotOff := got.Zone(); gotOff != off {
		t.Fatalf("offset = %d, want %d", gotOff, off)
	}
}
