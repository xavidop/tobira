package postgres

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/xavidop/tobira/driver"
)

// vectorTypeName is the type name of the pgvector extension. Its OID differs
// per database, so values are recognised by name.
const vectorTypeName = "vector"

// valueFromPG converts one raw column value, as received on the wire in the
// given format (pgtype.TextFormatCode or pgtype.BinaryFormatCode), to a
// driver.Value. typeName is the column's type name; it may be empty for types
// pgtype knows. A nil raw is NULL. Types tobira cannot model, interval for
// one, become driver.Unknown carrying the raw bytes and their text form.
func valueFromPG(m *pgtype.Map, oid uint32, format int16, typeName string, raw []byte) (driver.Value, error) {
	if raw == nil {
		return driver.Null(), nil
	}
	dt, known := m.TypeForOID(oid)
	if typeName == "" && known {
		typeName = dt.Name
	}
	if typeName == vectorTypeName {
		return vectorFromPG(format, raw)
	}
	if !known {
		return unknownFromPG(typeName, format, raw), nil
	}
	if ac, ok := dt.Codec.(*pgtype.ArrayCodec); ok {
		return listFromPG(m, oid, format, ac.ElementType, raw)
	}
	var err error
	switch oid {
	case pgtype.Int2OID, pgtype.Int4OID, pgtype.Int8OID:
		var n int64
		if err = m.Scan(oid, format, raw, &n); err == nil {
			return driver.Int(n), nil
		}
	case pgtype.Float4OID, pgtype.Float8OID:
		var f float64
		if err = m.Scan(oid, format, raw, &f); err == nil {
			return driver.Float(f), nil
		}
	case pgtype.BoolOID:
		var b bool
		if err = m.Scan(oid, format, raw, &b); err == nil {
			return driver.Bool(b), nil
		}
	case pgtype.TextOID, pgtype.VarcharOID, pgtype.BPCharOID, pgtype.NameOID:
		return driver.String(string(raw)), nil
	case pgtype.NumericOID:
		return decimalFromPG(m, format, raw)
	case pgtype.UUIDOID:
		var u pgtype.UUID
		if err = m.Scan(oid, format, raw, &u); err == nil {
			return driver.String(u.String()), nil
		}
	case pgtype.ByteaOID:
		var b []byte
		if err = m.Scan(oid, format, raw, &b); err == nil {
			return driver.Bytes(b), nil
		}
	case pgtype.TimestamptzOID:
		var ts pgtype.Timestamptz
		if err = m.Scan(oid, format, raw, &ts); err == nil {
			if ts.InfinityModifier != pgtype.Finite {
				return unknownFromPG(typeName, format, raw), nil
			}
			return driver.Time(ts.Time.UTC()), nil
		}
	case pgtype.TimestampOID:
		var ts pgtype.Timestamp
		if err = m.Scan(oid, format, raw, &ts); err == nil {
			if ts.InfinityModifier != pgtype.Finite {
				return unknownFromPG(typeName, format, raw), nil
			}
			return driver.Time(ts.Time.UTC()), nil
		}
	case pgtype.DateOID:
		var d pgtype.Date
		if err = m.Scan(oid, format, raw, &d); err == nil {
			if d.InfinityModifier != pgtype.Finite {
				return unknownFromPG(typeName, format, raw), nil
			}
			return driver.Time(d.Time.UTC()), nil
		}
	case pgtype.JSONOID, pgtype.JSONBOID:
		var b []byte
		if err = m.Scan(oid, format, raw, &b); err == nil {
			return driver.JSON(b), nil
		}
	default:
		return unknownFromPG(typeName, format, raw), nil
	}
	return driver.Null(), fmt.Errorf("decode %s: %w", typeName, err)
}

// unknownFromPG keeps the raw bytes of a type tobira does not model. The
// display text is the raw text; a binary value falls back to hex.
func unknownFromPG(typeName string, format int16, raw []byte) driver.Value {
	display := string(raw)
	if format == pgtype.BinaryFormatCode {
		display = `\x` + hex.EncodeToString(raw)
	}
	return driver.Unknown(typeName, raw, display)
}

// decimalFromPG keeps the numeric's display scale: 12.50 stays "12.50".
func decimalFromPG(m *pgtype.Map, format int16, raw []byte) (driver.Value, error) {
	if format == pgtype.TextFormatCode {
		return driver.Decimal(string(raw)), nil
	}
	var n pgtype.Numeric
	if err := m.Scan(pgtype.NumericOID, format, raw, &n); err != nil {
		return driver.Null(), fmt.Errorf("decode numeric: %w", err)
	}
	switch {
	case n.NaN:
		return driver.Decimal("NaN"), nil
	case n.InfinityModifier == pgtype.Infinity:
		return driver.Decimal("Infinity"), nil
	case n.InfinityModifier == pgtype.NegativeInfinity:
		return driver.Decimal("-Infinity"), nil
	case n.Int == nil:
		return driver.Decimal("0"), nil
	}
	return driver.Decimal(formatNumeric(n.Int, n.Exp)), nil
}

// formatNumeric renders int * 10^exp with exactly -exp fractional digits.
func formatNumeric(i *big.Int, exp int32) string {
	digits := new(big.Int).Abs(i).String()
	sign := ""
	if i.Sign() < 0 {
		sign = "-"
	}
	if exp >= 0 {
		return sign + digits + strings.Repeat("0", int(exp))
	}
	scale := int(-exp)
	if len(digits) <= scale {
		digits = strings.Repeat("0", scale-len(digits)+1) + digits
	}
	return sign + digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
}

// vectorFromPG parses pgvector's text form "[1,2,3]" or its binary form
// (int16 dimensions, int16 unused, then big-endian float32s).
func vectorFromPG(format int16, raw []byte) (driver.Value, error) {
	if format == pgtype.BinaryFormatCode {
		if len(raw) < 4 {
			return driver.Null(), fmt.Errorf("decode vector: short binary value")
		}
		dim := int(binary.BigEndian.Uint16(raw))
		if len(raw) != 4+4*dim {
			return driver.Null(), fmt.Errorf("decode vector: binary length %d does not match %d dimensions", len(raw), dim)
		}
		v := make([]float32, dim)
		for i := range v {
			v[i] = math.Float32frombits(binary.BigEndian.Uint32(raw[4+4*i:]))
		}
		return driver.Vector(v), nil
	}
	s := strings.TrimSpace(string(raw))
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return driver.Null(), fmt.Errorf("decode vector: malformed text %q", s)
	}
	s = strings.TrimSpace(s[1 : len(s)-1])
	v := []float32{}
	if s != "" {
		for _, part := range strings.Split(s, ",") {
			f, err := strconv.ParseFloat(strings.TrimSpace(part), 32)
			if err != nil {
				return driver.Null(), fmt.Errorf("decode vector: %w", err)
			}
			v = append(v, float32(f))
		}
	}
	return driver.Vector(v), nil
}

// arrayScanner collects the raw elements of an array so each can be decoded by
// valueFromPG. It implements pgtype.ArraySetter.
type arrayScanner struct {
	dims  []pgtype.ArrayDimension
	elems []pgtype.UndecodedBytes
}

func (a *arrayScanner) SetDimensions(dims []pgtype.ArrayDimension) error {
	a.dims = dims
	n := 0
	if len(dims) > 0 {
		n = 1
		for _, d := range dims {
			n *= int(d.Length)
		}
	}
	a.elems = make([]pgtype.UndecodedBytes, n)
	return nil
}

func (a *arrayScanner) ScanIndex(i int) any { return &a.elems[i] }
func (a *arrayScanner) ScanIndexType() any  { return new(pgtype.UndecodedBytes) }

// listFromPG decodes an array to a List, nesting one List per dimension.
func listFromPG(m *pgtype.Map, oid uint32, format int16, elemType *pgtype.Type, raw []byte) (driver.Value, error) {
	var arr arrayScanner
	if err := m.Scan(oid, format, raw, &arr); err != nil {
		return driver.Null(), fmt.Errorf("decode array: %w", err)
	}
	if len(arr.dims) == 0 {
		return driver.List(), nil
	}
	elemOID, elemName := elemType.OID, elemType.Name
	pos := 0
	var build func(dim int) (driver.Value, error)
	build = func(dim int) (driver.Value, error) {
		out := make([]driver.Value, 0, arr.dims[dim].Length)
		for range arr.dims[dim].Length {
			if dim == len(arr.dims)-1 {
				v, err := valueFromPG(m, elemOID, format, elemName, arr.elems[pos])
				if err != nil {
					return driver.Null(), err
				}
				pos++
				out = append(out, v)
				continue
			}
			v, err := build(dim + 1)
			if err != nil {
				return driver.Null(), err
			}
			out = append(out, v)
		}
		return driver.List(out...), nil
	}
	return build(0)
}

// paramToPG converts a query parameter to a Go value pgx can encode. A Decimal
// is sent as text so the server casts it to the parameter's type; a Vector as
// pgvector's text form; JSON and Bytes as []byte.
func paramToPG(v driver.Value) (any, error) {
	switch v.Type() {
	case driver.TypeNull:
		return nil, nil
	case driver.TypeBool:
		b, _ := v.AsBool()
		return b, nil
	case driver.TypeInt:
		n, _ := v.AsInt()
		return n, nil
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		return f, nil
	case driver.TypeDecimal, driver.TypeString, driver.TypeUnknown:
		return v.String(), nil
	case driver.TypeBytes:
		b, _ := v.AsBytes()
		return b, nil
	case driver.TypeTime:
		t, _ := v.AsTime()
		return t, nil
	case driver.TypeJSON:
		b, _ := v.AsJSON()
		return b, nil
	case driver.TypeVector:
		vec, _ := v.AsVector()
		parts := make([]string, len(vec))
		for i, f := range vec {
			parts[i] = strconv.FormatFloat(float64(f), 'g', -1, 32)
		}
		return "[" + strings.Join(parts, ",") + "]", nil
	case driver.TypeList:
		l, _ := v.AsList()
		out := make([]any, len(l))
		for i, e := range l {
			x, err := paramToPG(e)
			if err != nil {
				return nil, err
			}
			out[i] = x
		}
		return out, nil
	case driver.TypeObject:
		o, _ := v.AsObject()
		out := make(map[string]any, len(o))
		for _, m := range o {
			x, err := paramToPG(m.Value)
			if err != nil {
				return nil, err
			}
			out[m.Key] = x
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported parameter type %s", v.Type())
}
