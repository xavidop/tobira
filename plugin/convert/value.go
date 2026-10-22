package convert

import (
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/xavidop/tobira/driver"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
)

// The limits every conversion enforces (see the package doc). They match the
// host-side caps of the plugin protocol.
const (
	// MaxValueBytes caps one Value's protobuf encoding, nested members
	// included.
	MaxValueBytes = 1 << 20
	// MaxColumns caps the columns of one result set.
	MaxColumns = 4096
	// MaxNameBytes caps a column name, native type name, object key,
	// collection or namespace name, and time zone name.
	MaxNameBytes = 64 << 10
	// MaxDepth caps list/object nesting. A scalar is depth 0; a list of
	// scalars is depth 1.
	MaxDepth = 64
	// MaxMessageBytes is the gRPC message size cap both sides configure.
	MaxMessageBytes = 16 << 20
)

var (
	// ErrLimit means a value or message breaks one of the limits.
	ErrLimit = errors.New("convert: limit exceeded")
	// ErrInvalid means a message is malformed.
	ErrInvalid = errors.New("convert: invalid message")
)

func limitf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrLimit}, args...)...)
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

// checkName enforces MaxNameBytes on a name; what says which name it is.
func checkName(what, s string) error {
	if len(s) > MaxNameBytes {
		return limitf("%s is %d bytes, over %d", what, len(s), MaxNameBytes)
	}
	return nil
}

// checkSize enforces MaxValueBytes on an encoded value.
func checkSize(p *pb.Value) error {
	if n := proto.Size(p); n > MaxValueBytes {
		return limitf("value is %d bytes, over %d", n, MaxValueBytes)
	}
	return nil
}

// ValueToProto encodes v. It fails with ErrLimit when v breaks a limit.
func ValueToProto(v driver.Value) (*pb.Value, error) {
	p, err := valueToProto(v, 0)
	if err != nil {
		return nil, err
	}
	if err := checkSize(p); err != nil {
		return nil, err
	}
	return p, nil
}

func valueToProto(v driver.Value, depth int) (*pb.Value, error) {
	switch v.Type() {
	case driver.TypeNull:
		return &pb.Value{Kind: &pb.Value_Null{Null: &pb.Null{}}}, nil
	case driver.TypeBool:
		b, _ := v.AsBool()
		return &pb.Value{Kind: &pb.Value_BoolValue{BoolValue: b}}, nil
	case driver.TypeInt:
		n, _ := v.AsInt()
		return &pb.Value{Kind: &pb.Value_IntValue{IntValue: n}}, nil
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		return &pb.Value{Kind: &pb.Value_FloatValue{FloatValue: f}}, nil
	case driver.TypeDecimal:
		s, _ := v.AsText()
		return &pb.Value{Kind: &pb.Value_DecimalValue{DecimalValue: s}}, nil
	case driver.TypeString:
		s, _ := v.AsText()
		return &pb.Value{Kind: &pb.Value_StringValue{StringValue: s}}, nil
	case driver.TypeBytes:
		b, _ := v.AsBytes()
		return &pb.Value{Kind: &pb.Value_BytesValue{BytesValue: b}}, nil
	case driver.TypeTime:
		t, _ := v.AsTime()
		pt := TimeToProto(t)
		if err := checkName("time zone name", pt.GetZone()); err != nil {
			return nil, err
		}
		return &pb.Value{Kind: &pb.Value_TimeValue{TimeValue: pt}}, nil
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		return &pb.Value{Kind: &pb.Value_JsonValue{JsonValue: string(raw)}}, nil
	case driver.TypeVector:
		vec, _ := v.AsVector()
		return &pb.Value{Kind: &pb.Value_VectorValue{VectorValue: &pb.Vector{Values: vec}}}, nil
	case driver.TypeList:
		if depth >= MaxDepth {
			return nil, limitf("value nests deeper than %d", MaxDepth)
		}
		elems, _ := v.AsList()
		out := make([]*pb.Value, len(elems))
		for i, e := range elems {
			pe, err := valueToProto(e, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = pe
		}
		return &pb.Value{Kind: &pb.Value_ListValue{ListValue: &pb.List{Values: out}}}, nil
	case driver.TypeObject:
		if depth >= MaxDepth {
			return nil, limitf("value nests deeper than %d", MaxDepth)
		}
		o, _ := v.AsObject()
		po, err := objectToProto(o, depth+1)
		if err != nil {
			return nil, err
		}
		return &pb.Value{Kind: &pb.Value_ObjectValue{ObjectValue: po}}, nil
	case driver.TypeUnknown:
		native, raw, _ := v.AsUnknown()
		if err := checkName("native type name", native); err != nil {
			return nil, err
		}
		return &pb.Value{Kind: &pb.Value_UnknownValue{UnknownValue: &pb.Unknown{
			NativeType: native, Raw: raw, Display: v.String(),
		}}}, nil
	}
	return nil, invalidf("unknown value type %v", v.Type())
}

func objectToProto(o driver.Object, depth int) (*pb.Object, error) {
	members := make([]*pb.Member, len(o))
	for i, m := range o {
		if err := checkName("object key", m.Key); err != nil {
			return nil, err
		}
		pv, err := valueToProto(m.Value, depth)
		if err != nil {
			return nil, err
		}
		members[i] = &pb.Member{Key: m.Key, Value: pv}
	}
	return &pb.Object{Members: members}, nil
}

// ValueFromProto decodes p. A nil p, an unset oneof or a nil element is an
// ErrInvalid; a value over a limit is an ErrLimit. A set oneof whose
// sub-message is nil decodes as that type's empty value.
func ValueFromProto(p *pb.Value) (driver.Value, error) {
	if p == nil {
		return driver.Null(), invalidf("missing value")
	}
	if err := checkSize(p); err != nil {
		return driver.Null(), err
	}
	return valueFromProto(p, 0)
}

func valueFromProto(p *pb.Value, depth int) (driver.Value, error) {
	if p == nil {
		return driver.Null(), invalidf("missing value")
	}
	switch k := p.GetKind().(type) {
	case *pb.Value_Null:
		return driver.Null(), nil
	case *pb.Value_BoolValue:
		return driver.Bool(k.BoolValue), nil
	case *pb.Value_IntValue:
		return driver.Int(k.IntValue), nil
	case *pb.Value_FloatValue:
		return driver.Float(k.FloatValue), nil
	case *pb.Value_DecimalValue:
		return driver.Decimal(k.DecimalValue), nil
	case *pb.Value_StringValue:
		return driver.String(k.StringValue), nil
	case *pb.Value_BytesValue:
		return driver.Bytes(k.BytesValue), nil
	case *pb.Value_TimeValue:
		t, err := TimeFromProto(k.TimeValue)
		if err != nil {
			return driver.Null(), err
		}
		return driver.Time(t), nil
	case *pb.Value_JsonValue:
		return driver.JSON([]byte(k.JsonValue)), nil
	case *pb.Value_VectorValue:
		return driver.Vector(k.VectorValue.GetValues()), nil
	case *pb.Value_ListValue:
		if depth >= MaxDepth {
			return driver.Null(), limitf("value nests deeper than %d", MaxDepth)
		}
		elems := k.ListValue.GetValues()
		out := make([]driver.Value, len(elems))
		for i, e := range elems {
			v, err := valueFromProto(e, depth+1)
			if err != nil {
				return driver.Null(), err
			}
			out[i] = v
		}
		return driver.List(out...), nil
	case *pb.Value_ObjectValue:
		if depth >= MaxDepth {
			return driver.Null(), limitf("value nests deeper than %d", MaxDepth)
		}
		o, err := objectFromProto(k.ObjectValue, depth+1)
		if err != nil {
			return driver.Null(), err
		}
		return driver.ObjectValue(o...), nil
	case *pb.Value_UnknownValue:
		u := k.UnknownValue
		if err := checkName("native type name", u.GetNativeType()); err != nil {
			return driver.Null(), err
		}
		return driver.Unknown(u.GetNativeType(), u.GetRaw(), u.GetDisplay()), nil
	}
	return driver.Null(), invalidf("value has no kind")
}

func objectFromProto(p *pb.Object, depth int) (driver.Object, error) {
	members := p.GetMembers()
	out := make(driver.Object, len(members))
	for i, m := range members {
		if m == nil {
			return nil, invalidf("missing object member")
		}
		if err := checkName("object key", m.GetKey()); err != nil {
			return nil, err
		}
		v, err := valueFromProto(m.GetValue(), depth)
		if err != nil {
			return nil, err
		}
		out[i] = driver.Member{Key: m.GetKey(), Value: v}
	}
	return out, nil
}

// ValuesToProto encodes vs (query parameters, a row), each value under the
// limits.
func ValuesToProto(vs []driver.Value) ([]*pb.Value, error) {
	out := make([]*pb.Value, len(vs))
	for i, v := range vs {
		p, err := ValueToProto(v)
		if err != nil {
			return nil, fmt.Errorf("value %d: %w", i, err)
		}
		out[i] = p
	}
	return out, nil
}

// ValuesFromProto decodes ps, each value under the limits.
func ValuesFromProto(ps []*pb.Value) ([]driver.Value, error) {
	out := make([]driver.Value, len(ps))
	for i, p := range ps {
		v, err := ValueFromProto(p)
		if err != nil {
			return nil, fmt.Errorf("value %d: %w", i, err)
		}
		out[i] = v
	}
	return out, nil
}

// ObjectToProto encodes a key/value set such as a Mutator key or change
// set. Each member value is capped at MaxValueBytes on its own.
func ObjectToProto(o driver.Object) (*pb.Object, error) {
	members := make([]*pb.Member, len(o))
	for i, m := range o {
		if err := checkName("object key", m.Key); err != nil {
			return nil, err
		}
		pv, err := ValueToProto(m.Value)
		if err != nil {
			return nil, fmt.Errorf("member %q: %w", m.Key, err)
		}
		members[i] = &pb.Member{Key: m.Key, Value: pv}
	}
	return &pb.Object{Members: members}, nil
}

// ObjectFromProto decodes a key/value set; nil decodes as a nil Object. Each
// member value is capped at MaxValueBytes on its own.
func ObjectFromProto(p *pb.Object) (driver.Object, error) {
	if p == nil {
		return nil, nil
	}
	out := make(driver.Object, len(p.GetMembers()))
	for i, m := range p.GetMembers() {
		if m == nil {
			return nil, invalidf("missing object member")
		}
		if err := checkName("object key", m.GetKey()); err != nil {
			return nil, err
		}
		v, err := ValueFromProto(m.GetValue())
		if err != nil {
			return nil, fmt.Errorf("member %q: %w", m.GetKey(), err)
		}
		out[i] = driver.Member{Key: m.GetKey(), Value: v}
	}
	return out, nil
}

// zoneUTC and zoneLocal are the zone names that decode to time.UTC and
// time.Local rather than to a fixed zone.
const (
	zoneUTC   = "UTC"
	zoneLocal = "Local"
)

// maxOffset bounds a zone offset: real offsets lie within ±26h.
const maxOffset = 26 * 3600

// TimeToProto encodes t: its instant to the nanosecond, and its zone. A UTC
// time is sent as zone "UTC", a time.Local one as "Local", and any other as
// its zone abbreviation and offset at that instant ("CEST", 7200).
func TimeToProto(t time.Time) *pb.Time {
	name, off := t.Zone()
	switch t.Location() {
	case time.UTC:
		name = zoneUTC
	case time.Local:
		name = zoneLocal
	}
	return &pb.Time{
		Instant:       &timestamppb.Timestamp{Seconds: t.Unix(), Nanos: int32(t.Nanosecond())},
		Zone:          name,
		OffsetSeconds: int32(off),
	}
}

// TimeFromProto decodes p; nil, or a Time with no instant, decodes as the
// zero time. Zone "UTC" with offset 0 becomes time.UTC, and "Local" becomes
// time.Local when the offset matches the local zone at that instant.
// Anything else becomes a fixed zone with the sent name and offset, which
// keeps the instant, the wall clock and the abbreviation exactly.
//
// One case is not exact: a time.FixedZone the plugin happened to name
// "Local", with the host's local offset, comes back as time.Local. The
// instant and offset are unchanged; only later arithmetic across a DST
// change would differ.
func TimeFromProto(p *pb.Time) (time.Time, error) {
	if p.GetInstant() == nil {
		return time.Time{}, nil
	}
	ts := p.GetInstant()
	nanos := ts.GetNanos()
	if nanos < 0 || nanos >= 1e9 {
		return time.Time{}, invalidf("timestamp nanos %d out of range", nanos)
	}
	zone, off := p.GetZone(), int(p.GetOffsetSeconds())
	if err := checkName("time zone name", zone); err != nil {
		return time.Time{}, err
	}
	if off < -maxOffset || off > maxOffset {
		return time.Time{}, invalidf("zone offset %ds out of range", off)
	}
	t := time.Unix(ts.GetSeconds(), int64(nanos))
	switch {
	case zone == zoneUTC && off == 0:
		return t.In(time.UTC), nil
	case zone == zoneLocal:
		if _, localOff := t.In(time.Local).Zone(); localOff == off {
			return t.In(time.Local), nil
		}
	}
	return t.In(time.FixedZone(zone, off)), nil
}
