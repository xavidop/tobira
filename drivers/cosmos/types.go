package cosmos

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"

	"github.com/xavidop/tobira/driver"
)

// maxDepth caps how deeply nested a decoded document may be.
const maxDepth = 128

// errNotFinite refuses a value JSON cannot hold.
var errNotFinite = errors.New("NaN and infinities cannot be stored in Cosmos DB")

// errTooDeep is returned for a document nested beyond maxDepth.
var errTooDeep = fmt.Errorf("the document is nested deeper than %d levels", maxDepth)

// decodeValue decodes one JSON value as a driver.Value, keeping object key
// order. A number without a fraction or exponent is an Int (a Decimal
// holding its text when it overflows int64); any other number is a Float
// (a Decimal when it overflows float64). Trailing data is an error.
func decodeValue(raw []byte) (driver.Value, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeNext(dec, 0)
	if err != nil {
		return driver.Value{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return driver.Value{}, errors.New("trailing data after the JSON value")
	}
	return v, nil
}

func decodeNext(dec *json.Decoder, depth int) (driver.Value, error) {
	tok, err := dec.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return driver.Value{}, io.ErrUnexpectedEOF
		}
		return driver.Value{}, err
	}
	switch t := tok.(type) {
	case nil:
		return driver.Null(), nil
	case bool:
		return driver.Bool(t), nil
	case string:
		return driver.String(t), nil
	case json.Number:
		return numberValue(string(t)), nil
	case json.Delim:
		if depth >= maxDepth {
			return driver.Value{}, errTooDeep
		}
		switch t {
		case '[':
			var items []driver.Value
			for dec.More() {
				v, err := decodeNext(dec, depth+1)
				if err != nil {
					return driver.Value{}, err
				}
				items = append(items, v)
			}
			if _, err := dec.Token(); err != nil {
				return driver.Value{}, err
			}
			return driver.List(items...), nil
		case '{':
			var obj driver.Object
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return driver.Value{}, err
				}
				k, ok := kt.(string)
				if !ok {
					return driver.Value{}, errors.New("an object key is not a string")
				}
				v, err := decodeNext(dec, depth+1)
				if err != nil {
					return driver.Value{}, err
				}
				obj = append(obj, driver.Member{Key: k, Value: v})
			}
			if _, err := dec.Token(); err != nil {
				return driver.Value{}, err
			}
			return driver.ObjectValue(obj...), nil
		}
	}
	return driver.Value{}, fmt.Errorf("unexpected JSON token %v", tok)
}

// numberValue types a JSON number's text.
func numberValue(s string) driver.Value {
	if !strings.ContainsAny(s, ".eE") {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return driver.Int(n)
		}
		return driver.Decimal(s)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return driver.Decimal(s)
	}
	return driver.Float(f)
}

// encodeValue encodes v as the JSON Cosmos DB stores: Decimal as a JSON
// number (Cosmos DB keeps numbers as IEEE 754 doubles), Time as RFC 3339
// text, Bytes as base64, Vector as an array of numbers and JSON verbatim.
// Unknown values, NaN, infinities, a Decimal that is not a number and
// invalid JSON are ErrUnsupported.
func encodeValue(v driver.Value) ([]byte, error) {
	if err := encodable(v, 0); err != nil {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "encode", err)
	}
	return v.MarshalJSON()
}

func encodable(v driver.Value, depth int) error {
	if depth > maxDepth {
		return errTooDeep
	}
	switch v.Type() {
	case driver.TypeUnknown:
		nt, _, _ := v.AsUnknown()
		return fmt.Errorf("a %s value cannot be stored in Cosmos DB", nt)
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return errNotFinite
		}
	case driver.TypeDecimal:
		s, _ := v.AsText()
		if !json.Valid([]byte(s)) || strings.ContainsAny(s, `"{[tfn`) {
			return fmt.Errorf("decimal %q is not a number", s)
		}
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		if !json.Valid(raw) {
			return errors.New("the JSON value is not valid JSON")
		}
	case driver.TypeVector:
		vec, _ := v.AsVector()
		for _, f := range vec {
			if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
				return errNotFinite
			}
		}
	case driver.TypeList:
		items, _ := v.AsList()
		for _, e := range items {
			if err := encodable(e, depth+1); err != nil {
				return err
			}
		}
	case driver.TypeObject:
		obj, _ := v.AsObject()
		for _, m := range obj {
			if err := encodable(m.Value, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// rawJSON encodes v for a query parameter; its error is unclassified.
func rawJSON(v driver.Value) (json.RawMessage, error) {
	if err := encodable(v, 0); err != nil {
		return nil, err
	}
	b, err := v.MarshalJSON()
	return json.RawMessage(b), err
}

// pathSegments splits a Cosmos DB path such as /a/b or /"odd name" into
// its property names.
func pathSegments(path string) []string {
	var out []string
	for seg := range strings.SplitSeq(strings.TrimPrefix(path, "/"), "/") {
		if len(seg) >= 2 && seg[0] == '"' && seg[len(seg)-1] == '"' {
			seg = seg[1 : len(seg)-1]
		}
		out = append(out, seg)
	}
	return out
}

// pkFieldName is the field name tobira uses for a partition key (or
// vector) path: its property names joined with dots ("/a/b" is "a.b").
func pkFieldName(path string) string { return strings.Join(pathSegments(path), ".") }

// pkValueAt returns the value at a path in doc.
func pkValueAt(doc driver.Object, path string) (driver.Value, bool) {
	cur := driver.ObjectValue(doc...)
	for _, seg := range pathSegments(path) {
		obj, ok := cur.AsObject()
		if !ok {
			return driver.Value{}, false
		}
		if cur, ok = obj.Get(seg); !ok {
			return driver.Value{}, false
		}
	}
	return cur, true
}

// partitionKeyOf builds a partition key from its component values (one per
// path; several for a hierarchical key). Strings, numbers, booleans and
// null are allowed.
func partitionKeyOf(values []driver.Value) (azcosmos.PartitionKey, error) {
	pk := azcosmos.NewPartitionKey()
	for _, v := range values {
		switch v.Type() {
		case driver.TypeNull:
			pk = pk.AppendNull()
		case driver.TypeString:
			s, _ := v.AsText()
			pk = pk.AppendString(s)
		case driver.TypeBool:
			b, _ := v.AsBool()
			pk = pk.AppendBool(b)
		case driver.TypeInt, driver.TypeFloat:
			f, _ := v.AsFloat()
			pk = pk.AppendNumber(f)
		case driver.TypeDecimal:
			s, _ := v.AsText()
			f, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return pk, driver.Wrap(driver.ErrUnsupported, Name, "partition key", fmt.Errorf("decimal %q is not a number", s))
			}
			pk = pk.AppendNumber(f)
		default:
			return pk, driver.Wrap(driver.ErrUnsupported, Name, "partition key",
				fmt.Errorf("a partition key value must be a string, number, boolean or null, not %s", v.Type()))
		}
	}
	return pk, nil
}

// partitionKeyFromJSON builds a partition key from decoded JSON values (an
// envelope's partitionKey).
func partitionKeyFromJSON(values []any) (azcosmos.PartitionKey, error) {
	pk := azcosmos.NewPartitionKey()
	for _, v := range values {
		switch t := v.(type) {
		case nil:
			pk = pk.AppendNull()
		case string:
			pk = pk.AppendString(t)
		case bool:
			pk = pk.AppendBool(t)
		case float64:
			pk = pk.AppendNumber(t)
		case json.Number:
			f, err := t.Float64()
			if err != nil {
				return pk, driver.Wrap(driver.ErrSyntax, Name, "query", fmt.Errorf("partitionKey number %s is out of range", t))
			}
			pk = pk.AppendNumber(f)
		default:
			return pk, driver.Wrap(driver.ErrSyntax, Name, "query", errors.New("a partitionKey value must be a string, number, boolean or null"))
		}
	}
	return pk, nil
}
