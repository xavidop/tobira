package cassandra

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net"
	"reflect"
	"strconv"
	"strings"
	"time"

	gocql "github.com/apache/cassandra-gocql-driver/v2"
	"gopkg.in/inf.v0"

	"github.com/xavidop/tobira/driver"
)

// cell receives one column of a row: gocql hands it the raw bytes, which
// it decodes at once (the bytes belong to gocql's frame).
type cell struct {
	v driver.Value
}

func (c *cell) UnmarshalCQL(info gocql.TypeInfo, data []byte) error {
	v, err := decodeValue(info, data)
	c.v = v
	return err
}

// decodeValue decodes one value of type info. nil data is NULL; an empty
// collection (which only a frozen collection keeps) is an empty List or
// Object.
func decodeValue(info gocql.TypeInfo, data []byte) (driver.Value, error) {
	return decode(info, data, 0)
}

func decode(info gocql.TypeInfo, data []byte, depth int) (driver.Value, error) {
	if data == nil {
		return driver.Null(), nil
	}
	if len(data) == 0 && !emptyAllowed(info) {
		// An empty value of a fixed-width or composite type: legal in CQL
		// (blobAsInt(0x)) and common in data written over Thrift. gocql
		// reads it as the zero value; tobira shows it as NULL rather than
		// invent a 0 or fail the whole result.
		return driver.Null(), nil
	}
	if depth > maxDepth {
		return driver.Unknown(nativeName(info), nil, fmt.Sprintf("<%s nested too deep to show>", nativeName(info))), nil
	}
	fixed := func(n int) error {
		if len(data) != n {
			return fmt.Errorf("%s: want %d bytes, got %d", nativeName(info), n, len(data))
		}
		return nil
	}
	switch v := info.(type) {
	case gocql.CollectionType:
		return decodeCollection(v, data, depth)
	case gocql.TupleTypeInfo:
		r := &reader{b: data}
		out := make([]driver.Value, len(v.Elems))
		for i, e := range v.Elems {
			if out[i], _ = r.field(e, depth); r.err != nil {
				return driver.Value{}, r.err
			}
		}
		return driver.List(out...), r.end(info)
	case gocql.UDTTypeInfo:
		r := &reader{b: data}
		out := make(driver.Object, len(v.Elements))
		for i, e := range v.Elements {
			val, _ := r.field(e.Type, depth)
			if r.err != nil {
				return driver.Value{}, r.err
			}
			out[i] = driver.Member{Key: e.Name, Value: val}
		}
		return driver.ObjectValue(out...), r.end(info)
	case gocql.VectorType:
		return decodeVector(v, data, depth)
	}
	switch info.Type() {
	case gocql.TypeAscii, gocql.TypeVarchar, gocql.TypeText:
		return driver.String(string(data)), nil
	case gocql.TypeBlob:
		return driver.Bytes(data), nil
	case gocql.TypeBoolean:
		if err := fixed(1); err != nil {
			return driver.Value{}, err
		}
		return driver.Bool(data[0] != 0), nil
	case gocql.TypeTinyInt:
		if err := fixed(1); err != nil {
			return driver.Value{}, err
		}
		return driver.Int(int64(int8(data[0]))), nil
	case gocql.TypeSmallInt:
		if err := fixed(2); err != nil {
			return driver.Value{}, err
		}
		return driver.Int(int64(int16(binary.BigEndian.Uint16(data)))), nil
	case gocql.TypeInt:
		if err := fixed(4); err != nil {
			return driver.Value{}, err
		}
		return driver.Int(int64(int32(binary.BigEndian.Uint32(data)))), nil
	case gocql.TypeBigInt, gocql.TypeCounter:
		if err := fixed(8); err != nil {
			return driver.Value{}, err
		}
		return driver.Int(int64(binary.BigEndian.Uint64(data))), nil
	case gocql.TypeFloat:
		if err := fixed(4); err != nil {
			return driver.Value{}, err
		}
		f := math.Float32frombits(binary.BigEndian.Uint32(data))
		// The shortest text of the float32, so 0.1 reads 0.1.
		d, _ := strconv.ParseFloat(strconv.FormatFloat(float64(f), 'g', -1, 32), 64)
		return driver.Float(d), nil
	case gocql.TypeDouble:
		if err := fixed(8); err != nil {
			return driver.Value{}, err
		}
		return driver.Float(math.Float64frombits(binary.BigEndian.Uint64(data))), nil
	case gocql.TypeDecimal:
		var d inf.Dec
		if err := gocql.Unmarshal(info, data, &d); err != nil {
			return driver.Value{}, err
		}
		return driver.Decimal(d.String()), nil
	case gocql.TypeVarint:
		var b big.Int
		if err := gocql.Unmarshal(info, data, &b); err != nil {
			return driver.Value{}, err
		}
		return driver.Decimal(b.String()), nil
	case gocql.TypeTimestamp:
		if err := fixed(8); err != nil {
			return driver.Value{}, err
		}
		return driver.Time(time.UnixMilli(int64(binary.BigEndian.Uint64(data))).UTC()), nil
	case gocql.TypeDate:
		if err := fixed(4); err != nil {
			return driver.Value{}, err
		}
		days := int64(binary.BigEndian.Uint32(data)) - 1<<31
		return driver.Time(time.Unix(days*86400, 0).UTC()), nil
	case gocql.TypeTime:
		if err := fixed(8); err != nil {
			return driver.Value{}, err
		}
		return driver.String(formatTimeOfDay(time.Duration(binary.BigEndian.Uint64(data)))), nil
	case gocql.TypeUUID, gocql.TypeTimeUUID:
		u, err := gocql.UUIDFromBytes(data)
		if err != nil {
			return driver.Value{}, err
		}
		return driver.String(u.String()), nil
	case gocql.TypeInet:
		if len(data) != 4 && len(data) != 16 {
			return driver.Value{}, fmt.Errorf("inet: want 4 or 16 bytes, got %d", len(data))
		}
		return driver.String(net.IP(data).String()), nil
	case gocql.TypeDuration:
		var d gocql.Duration
		if err := gocql.Unmarshal(info, data, &d); err != nil {
			return driver.Value{}, err
		}
		return driver.String(formatDuration(d)), nil
	}
	return driver.Unknown(nativeName(info), data, rawDisplay(data)), nil
}

// emptyAllowed reports whether a zero-length value of info is a real
// value: an empty text or blob.
func emptyAllowed(info gocql.TypeInfo) bool {
	switch info.Type() {
	case gocql.TypeAscii, gocql.TypeVarchar, gocql.TypeText, gocql.TypeBlob:
		return true
	}
	return false
}

// rawDisplay shows bytes of a type tobira cannot model.
func rawDisplay(data []byte) string {
	const show = 64
	if len(data) > show {
		return "0x" + hex.EncodeToString(data[:show]) + "…"
	}
	return "0x" + hex.EncodeToString(data)
}

// tooLarge is the placeholder for a collection over maxCollectionElems.
func tooLarge(info gocql.TypeInfo, n int) driver.Value {
	return driver.Unknown(nativeName(info), nil,
		fmt.Sprintf("<%s of %d elements: too large to show (more than %d); select fewer rows or slice it>", nativeName(info), n, maxCollectionElems))
}

func decodeCollection(c gocql.CollectionType, data []byte, depth int) (driver.Value, error) {
	r := &reader{b: data}
	n := r.int32()
	switch {
	case r.err != nil:
		return driver.Value{}, r.err
	case n < 0:
		return driver.Value{}, fmt.Errorf("%s: negative size %d", nativeName(c), n)
	case n > maxCollectionElems:
		return tooLarge(c, n), nil
	}
	if c.Type() == gocql.TypeMap {
		out := make(driver.Object, 0, n)
		for range n {
			k, _ := r.elem(c.Key, depth)
			v, _ := r.elem(c.Elem, depth)
			if r.err != nil {
				return driver.Value{}, r.err
			}
			key, ok := k.AsText()
			if !ok || k.Type() != driver.TypeString {
				key = k.String()
			}
			out = append(out, driver.Member{Key: key, Value: v})
		}
		return driver.ObjectValue(out...), r.end(c)
	}
	out := make([]driver.Value, 0, n)
	for range n {
		v, _ := r.elem(c.Elem, depth)
		if r.err != nil {
			return driver.Value{}, r.err
		}
		out = append(out, v)
	}
	return driver.List(out...), r.end(c)
}

// decodeVector reads a vector: a float vector as a Vector, any other as a
// List, element by element through the element type's decoder.
func decodeVector(v gocql.VectorType, data []byte, depth int) (driver.Value, error) {
	if v.Dimensions > maxCollectionElems {
		return tooLarge(v, v.Dimensions), nil
	}
	if v.SubType.Type() == gocql.TypeFloat {
		if len(data) != 4*v.Dimensions {
			return driver.Value{}, fmt.Errorf("%s: want %d bytes, got %d", nativeName(v), 4*v.Dimensions, len(data))
		}
		out := make([]float32, v.Dimensions)
		for i := range out {
			out[i] = math.Float32frombits(binary.BigEndian.Uint32(data[4*i:]))
		}
		return driver.Vector(out), nil
	}
	zero := v.Zero()
	if zero == nil {
		return driver.Unknown(nativeName(v), data, rawDisplay(data)), nil
	}
	ptr := reflect.New(reflect.TypeOf(zero))
	if err := gocql.Unmarshal(v, data, ptr.Interface()); err != nil {
		return driver.Value{}, err
	}
	elems := ptr.Elem()
	out := make([]driver.Value, elems.Len())
	for i := range out {
		b, err := gocql.Marshal(v.SubType, elems.Index(i).Interface())
		if err != nil {
			return driver.Value{}, err
		}
		if out[i], err = decode(v.SubType, b, depth+1); err != nil {
			return driver.Value{}, err
		}
	}
	return driver.List(out...), nil
}

// reader reads the [int32 length][bytes] fields of a collection, tuple or
// UDT value. The first error sticks.
type reader struct {
	b   []byte
	err error
}

func (r *reader) int32() int {
	if r.err != nil {
		return 0
	}
	if len(r.b) < 4 {
		r.err = errors.New("truncated value")
		return 0
	}
	n := int32(binary.BigEndian.Uint32(r.b))
	r.b = r.b[4:]
	return int(n)
}

// field reads one length-prefixed field of a tuple or UDT and decodes it;
// a negative length, or the end of the value, is NULL.
func (r *reader) field(info gocql.TypeInfo, depth int) (driver.Value, error) {
	if r.err != nil || len(r.b) == 0 {
		return driver.Null(), r.err
	}
	return r.elem(info, depth)
}

// elem reads one length-prefixed element of a collection, which must be
// there; a negative length is NULL.
func (r *reader) elem(info gocql.TypeInfo, depth int) (driver.Value, error) {
	n := r.int32()
	if r.err != nil || n < 0 {
		return driver.Null(), r.err
	}
	if n > len(r.b) {
		r.err = errors.New("truncated value")
		return driver.Null(), r.err
	}
	v, err := decode(info, r.b[:n], depth+1)
	r.b = r.b[n:]
	if err != nil {
		r.err = err
	}
	return v, r.err
}

// end fails when bytes are left over.
func (r *reader) end(info gocql.TypeInfo) error {
	if r.err == nil && len(r.b) > 0 {
		r.err = fmt.Errorf("%s: %d bytes left over", nativeName(info), len(r.b))
	}
	return r.err
}

// formatTimeOfDay spells a CQL time: 15:04:05 with as many fractional
// digits as it has.
func formatTimeOfDay(d time.Duration) string {
	h, d := d/time.Hour, d%time.Hour
	m, d := d/time.Minute, d%time.Minute
	s, ns := d/time.Second, d%time.Second
	out := fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	if ns > 0 {
		out += strings.TrimRight(fmt.Sprintf(".%09d", ns), "0")
	}
	return out
}
