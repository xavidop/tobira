package mongodb

import (
	"encoding/hex"
	"fmt"
	"math"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xavidop/tobira/driver"
)

// The native names of BSON types, as MongoDB's $type operator spells them,
// plus "uuid" for a binary of subtype 4.
var nativeNames = map[bson.Type]string{
	bson.TypeDouble:           "double",
	bson.TypeString:           "string",
	bson.TypeEmbeddedDocument: "object",
	bson.TypeArray:            "array",
	bson.TypeBinary:           "binData",
	bson.TypeUndefined:        "undefined",
	bson.TypeObjectID:         "objectId",
	bson.TypeBoolean:          "bool",
	bson.TypeDateTime:         "date",
	bson.TypeNull:             "null",
	bson.TypeRegex:            "regex",
	bson.TypeDBPointer:        "dbPointer",
	bson.TypeJavaScript:       "javascript",
	bson.TypeSymbol:           "symbol",
	bson.TypeCodeWithScope:    "javascriptWithScope",
	bson.TypeInt32:            "int",
	bson.TypeTimestamp:        "timestamp",
	bson.TypeInt64:            "long",
	bson.TypeDecimal128:       "decimal",
	bson.TypeMinKey:           "minKey",
	bson.TypeMaxKey:           "maxKey",
}

// unknownTypes maps the native name of an Unknown value back to its BSON
// type, so it is written back exactly as it was read.
var unknownTypes = map[string]bson.Type{
	"objectId":            bson.TypeObjectID,
	"uuid":                bson.TypeBinary,
	"binData":             bson.TypeBinary,
	"regex":               bson.TypeRegex,
	"dbPointer":           bson.TypeDBPointer,
	"javascript":          bson.TypeJavaScript,
	"javascriptWithScope": bson.TypeCodeWithScope,
	"timestamp":           bson.TypeTimestamp,
	"minKey":              bson.TypeMinKey,
	"maxKey":              bson.TypeMaxKey,
}

// nativeName returns the BSON type name of rv: "int", "objectId", "uuid"….
func nativeName(rv bson.RawValue) string {
	if isUUID(rv) {
		return "uuid"
	}
	if n, ok := nativeNames[rv.Type]; ok {
		return n
	}
	return fmt.Sprintf("bson(0x%02x)", byte(rv.Type))
}

func isUUID(rv bson.RawValue) bool {
	if rv.Type != bson.TypeBinary {
		return false
	}
	sub, data, ok := rv.BinaryOK()
	return ok && sub == bson.TypeBinaryUUID && len(data) == 16
}

// valueOf maps a BSON value to a driver.Value:
//
//   - double → Float; int and long → Int; decimal → Decimal (its exact text);
//   - string and symbol → String; bool → Bool; date → Time (UTC);
//   - null and undefined → Null; object → Object (key order kept);
//     array → List; binData of subtype 0 → Bytes;
//   - objectId, uuid (binData subtype 4), any other binData, timestamp,
//     regex, javascript, dbPointer, minKey and maxKey → Unknown, carrying
//     the native name and the raw BSON value (so it is written back
//     unchanged) and a display text: the hex of an objectId, the canonical
//     form of a uuid, extended JSON otherwise.
//
// Malformed BSON becomes an Unknown too; valueOf never panics.
func valueOf(rv bson.RawValue) driver.Value {
	if err := rv.Validate(); err != nil {
		return driver.Unknown(nativeName(rv), rv.Value, "invalid BSON")
	}
	switch rv.Type {
	case bson.TypeDouble:
		return driver.Float(rv.Double())
	case bson.TypeString:
		return driver.String(rv.StringValue())
	case bson.TypeSymbol:
		return driver.String(rv.Symbol())
	case bson.TypeInt32:
		return driver.Int(int64(rv.Int32()))
	case bson.TypeInt64:
		return driver.Int(rv.Int64())
	case bson.TypeDecimal128:
		return driver.Decimal(rv.Decimal128().String())
	case bson.TypeBoolean:
		return driver.Bool(rv.Boolean())
	case bson.TypeDateTime:
		return driver.Time(rv.Time().UTC())
	case bson.TypeNull, bson.TypeUndefined:
		return driver.Null()
	case bson.TypeEmbeddedDocument:
		return driver.ObjectValue(objectOf(rv.Document())...)
	case bson.TypeArray:
		vals, err := rv.Array().Values()
		if err != nil {
			return driver.Unknown("array", rv.Value, "invalid BSON")
		}
		out := make([]driver.Value, len(vals))
		for i, e := range vals {
			out[i] = valueOf(e)
		}
		return driver.List(out...)
	case bson.TypeBinary:
		sub, data := rv.Binary()
		switch {
		case sub == bson.TypeBinaryGeneric:
			return driver.Bytes(data)
		case isUUID(rv):
			return driver.Unknown("uuid", rv.Value, formatUUID(data))
		}
	case bson.TypeObjectID:
		return driver.Unknown("objectId", rv.Value, rv.ObjectID().Hex())
	case bson.TypeRegex:
		p, o := rv.Regex()
		return driver.Unknown("regex", rv.Value, "/"+p+"/"+o)
	case bson.TypeJavaScript:
		return driver.Unknown("javascript", rv.Value, rv.JavaScript())
	case bson.TypeMinKey:
		return driver.Unknown("minKey", rv.Value, "MinKey")
	case bson.TypeMaxKey:
		return driver.Unknown("maxKey", rv.Value, "MaxKey")
	}
	return driver.Unknown(nativeName(rv), rv.Value, rv.String())
}

// objectOf maps a BSON document to an Object, keeping key order.
func objectOf(doc bson.Raw) driver.Object {
	elems, err := doc.Elements()
	if err != nil {
		return driver.Object{{Key: "", Value: driver.Unknown("object", doc, "invalid BSON")}}
	}
	out := make(driver.Object, len(elems))
	for i, e := range elems {
		out[i] = driver.Member{Key: e.Key(), Value: valueOf(e.Value())}
	}
	return out
}

func formatUUID(b []byte) string {
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// toBSON maps a driver.Value to a value the BSON encoder writes: the inverse
// of valueOf. Int is written as an int when it fits in 32 bits and a long
// otherwise; Bytes as binData subtype 0; Vector as an array of doubles; JSON
// is read as (relaxed) extended JSON; an Unknown is written back as the raw
// BSON value it carries. A Decimal that does not parse or JSON that does not
// parse is ErrSyntax; an Unknown of a type tobira did not read is
// ErrUnsupported.
func toBSON(v driver.Value) (any, error) {
	switch v.Type() {
	case driver.TypeNull:
		return nil, nil
	case driver.TypeBool:
		b, _ := v.AsBool()
		return b, nil
	case driver.TypeInt:
		n, _ := v.AsInt()
		if n >= math.MinInt32 && n <= math.MaxInt32 {
			return int32(n), nil
		}
		return n, nil
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		return f, nil
	case driver.TypeDecimal:
		s, _ := v.AsText()
		d, err := bson.ParseDecimal128(s)
		if err != nil {
			return nil, driver.Wrap(driver.ErrSyntax, Name, "encode", fmt.Errorf("decimal %q: %w", s, err))
		}
		return d, nil
	case driver.TypeString:
		s, _ := v.AsText()
		return s, nil
	case driver.TypeBytes:
		b, _ := v.AsBytes()
		return bson.Binary{Subtype: bson.TypeBinaryGeneric, Data: b}, nil
	case driver.TypeTime:
		t, _ := v.AsTime()
		return bson.NewDateTimeFromTime(t), nil
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		return parseJSONValue(raw)
	case driver.TypeVector:
		vec, _ := v.AsVector()
		out := make(bson.A, len(vec))
		for i, f := range vec {
			out[i] = float64(f)
		}
		return out, nil
	case driver.TypeList:
		l, _ := v.AsList()
		out := make(bson.A, len(l))
		for i, e := range l {
			x, err := toBSON(e)
			if err != nil {
				return nil, err
			}
			out[i] = x
		}
		return out, nil
	case driver.TypeObject:
		o, _ := v.AsObject()
		return objectToBSON(o)
	case driver.TypeUnknown:
		native, raw, _ := v.AsUnknown()
		t, ok := unknownTypes[native]
		if !ok {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, "encode", fmt.Errorf("cannot write a value of native type %q", native))
		}
		rv := bson.RawValue{Type: t, Value: raw}
		if err := rv.Validate(); err != nil {
			return nil, driver.Wrap(driver.ErrSyntax, Name, "encode", fmt.Errorf("invalid %s value: %w", native, err))
		}
		return rv, nil
	}
	return nil, driver.Wrap(driver.ErrUnsupported, Name, "encode", fmt.Errorf("cannot write a %s value", v.Type()))
}

// objectToBSON maps an Object to a BSON document, keeping key order.
func objectToBSON(o driver.Object) (bson.D, error) {
	out := make(bson.D, len(o))
	for i, m := range o {
		x, err := toBSON(m.Value)
		if err != nil {
			return nil, err
		}
		out[i] = bson.E{Key: m.Key, Value: x}
	}
	return out, nil
}

// parseJSONValue reads one (relaxed or canonical) extended JSON value.
func parseJSONValue(raw []byte) (any, error) {
	var d bson.D
	wrapped := append(append([]byte(`{"v":`), raw...), '}')
	if err := bson.UnmarshalExtJSON(wrapped, false, &d); err != nil || len(d) != 1 {
		if err == nil {
			err = fmt.Errorf("not a single value")
		}
		return nil, driver.Wrap(driver.ErrSyntax, Name, "encode", fmt.Errorf("invalid JSON value: %w", err))
	}
	return d[0].Value, nil
}
