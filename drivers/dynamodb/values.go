package dynamodb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/xavidop/tobira/driver"
)

// maxDepth is DynamoDB's nesting limit for documents (32 levels).
const maxDepth = 32

// valueOf converts an attribute value:
//
//   - S as String, B as Bytes, BOOL as Bool, NULL (and a nil value) as Null;
//   - N as Int when it is a whole number within int64, else Decimal (the
//     exact text DynamoDB returned);
//   - M as Object, keys sorted (DynamoDB keeps no order); L as List;
//   - SS, NS and BS as a List, sorted (a set has no order; numbers by
//     value). NativeType keeps saying SS, NS or BS; writing the List back
//     stores an L.
func valueOf(av types.AttributeValue) driver.Value {
	switch v := av.(type) {
	case *types.AttributeValueMemberS:
		return driver.String(v.Value)
	case *types.AttributeValueMemberN:
		return numberValue(v.Value)
	case *types.AttributeValueMemberB:
		return driver.Bytes(v.Value)
	case *types.AttributeValueMemberBOOL:
		return driver.Bool(v.Value)
	case *types.AttributeValueMemberNULL:
		return driver.Null()
	case *types.AttributeValueMemberM:
		keys := sortedKeys(v.Value)
		ms := make([]driver.Member, len(keys))
		for i, k := range keys {
			ms[i] = driver.Member{Key: k, Value: valueOf(v.Value[k])}
		}
		return driver.ObjectValue(ms...)
	case *types.AttributeValueMemberL:
		vs := make([]driver.Value, len(v.Value))
		for i, e := range v.Value {
			vs[i] = valueOf(e)
		}
		return driver.List(vs...)
	case *types.AttributeValueMemberSS:
		ss := slices.Sorted(slices.Values(v.Value))
		vs := make([]driver.Value, len(ss))
		for i, e := range ss {
			vs[i] = driver.String(e)
		}
		return driver.List(vs...)
	case *types.AttributeValueMemberNS:
		ns := slices.Clone(v.Value)
		slices.SortFunc(ns, compareNumbers)
		vs := make([]driver.Value, len(ns))
		for i, e := range ns {
			vs[i] = numberValue(e)
		}
		return driver.List(vs...)
	case *types.AttributeValueMemberBS:
		bs := slices.Clone(v.Value)
		slices.SortFunc(bs, bytes.Compare)
		vs := make([]driver.Value, len(bs))
		for i, e := range bs {
			vs[i] = driver.Bytes(e)
		}
		return driver.List(vs...)
	}
	return driver.Null()
}

// nativeType names an attribute value's DynamoDB type ("" for nil).
func nativeType(av types.AttributeValue) string {
	switch av.(type) {
	case *types.AttributeValueMemberS:
		return "S"
	case *types.AttributeValueMemberN:
		return "N"
	case *types.AttributeValueMemberB:
		return "B"
	case *types.AttributeValueMemberBOOL:
		return "BOOL"
	case *types.AttributeValueMemberNULL:
		return "NULL"
	case *types.AttributeValueMemberM:
		return "M"
	case *types.AttributeValueMemberL:
		return "L"
	case *types.AttributeValueMemberSS:
		return "SS"
	case *types.AttributeValueMemberNS:
		return "NS"
	case *types.AttributeValueMemberBS:
		return "BS"
	}
	return ""
}

// typeOfNative is the tobira type of a DynamoDB type name, for an N whose
// values are not known (Decimal: exact).
func typeOfNative(n string) driver.Type {
	switch n {
	case "S":
		return driver.TypeString
	case "N":
		return driver.TypeDecimal
	case "B":
		return driver.TypeBytes
	case "BOOL":
		return driver.TypeBool
	case "NULL":
		return driver.TypeNull
	case "M":
		return driver.TypeObject
	case "L", "SS", "NS", "BS":
		return driver.TypeList
	}
	return driver.TypeUnknown
}

// numberValue reads a DynamoDB number: Int when whole and within int64,
// else Decimal holding the text.
func numberValue(s string) driver.Value {
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return driver.Int(i)
	}
	if r, ok := new(big.Rat).SetString(s); ok && r.IsInt() && r.Num().IsInt64() {
		return driver.Int(r.Num().Int64())
	}
	return driver.Decimal(s)
}

// compareNumbers orders two DynamoDB number texts by value.
func compareNumbers(a, b string) int {
	ra, oka := new(big.Rat).SetString(a)
	rb, okb := new(big.Rat).SetString(b)
	if !oka || !okb {
		return strings.Compare(a, b)
	}
	return ra.Cmp(rb)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

var decimalRE = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)

// toAV converts a tobira value for DynamoDB: Null as NULL, Bool as BOOL,
// Int, Float and Decimal as N, String as S, Bytes as B, Time as an
// RFC 3339 S in UTC (DynamoDB has no time type), JSON, List and Object as
// L and M (JSON numbers stay exact), Vector as an L of N. An Unknown value,
// a NaN or infinite float, or nesting deeper than 32 levels is refused.
func toAV(v driver.Value) (types.AttributeValue, error) {
	return toAVDepth(v, 0)
}

func toAVDepth(v driver.Value, depth int) (types.AttributeValue, error) {
	if depth > maxDepth {
		return nil, valueErr(driver.ErrUnsupported, errors.New("the value nests deeper than DynamoDB's 32 levels"))
	}
	switch v.Type() {
	case driver.TypeNull:
		return &types.AttributeValueMemberNULL{Value: true}, nil
	case driver.TypeBool:
		b, _ := v.AsBool()
		return &types.AttributeValueMemberBOOL{Value: b}, nil
	case driver.TypeInt:
		i, _ := v.AsInt()
		return &types.AttributeValueMemberN{Value: strconv.FormatInt(i, 10)}, nil
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, valueErr(driver.ErrUnsupported, fmt.Errorf("DynamoDB numbers cannot hold %v", f))
		}
		return &types.AttributeValueMemberN{Value: strconv.FormatFloat(f, 'g', -1, 64)}, nil
	case driver.TypeDecimal:
		s, _ := v.AsText()
		if !decimalRE.MatchString(s) {
			return nil, valueErr(driver.ErrSyntax, fmt.Errorf("invalid number %q", s))
		}
		return &types.AttributeValueMemberN{Value: s}, nil
	case driver.TypeString:
		s, _ := v.AsText()
		return &types.AttributeValueMemberS{Value: s}, nil
	case driver.TypeBytes:
		b, _ := v.AsBytes()
		return &types.AttributeValueMemberB{Value: b}, nil
	case driver.TypeTime:
		t, _ := v.AsTime()
		return &types.AttributeValueMemberS{Value: t.UTC().Format(time.RFC3339Nano)}, nil
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var x any
		if err := dec.Decode(&x); err != nil || dec.More() {
			return nil, valueErr(driver.ErrSyntax, errors.New("invalid JSON value"))
		}
		return jsonAV(x, depth)
	case driver.TypeVector:
		vec, _ := v.AsVector()
		out := make([]types.AttributeValue, len(vec))
		for i, f := range vec {
			if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
				return nil, valueErr(driver.ErrUnsupported, fmt.Errorf("DynamoDB numbers cannot hold %v", f))
			}
			out[i] = &types.AttributeValueMemberN{Value: strconv.FormatFloat(float64(f), 'g', -1, 32)}
		}
		return &types.AttributeValueMemberL{Value: out}, nil
	case driver.TypeList:
		list, _ := v.AsList()
		out := make([]types.AttributeValue, len(list))
		for i, e := range list {
			av, err := toAVDepth(e, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = av
		}
		return &types.AttributeValueMemberL{Value: out}, nil
	case driver.TypeObject:
		obj, _ := v.AsObject()
		out := make(map[string]types.AttributeValue, len(obj))
		for _, m := range obj {
			av, err := toAVDepth(m.Value, depth+1)
			if err != nil {
				return nil, err
			}
			out[m.Key] = av
		}
		return &types.AttributeValueMemberM{Value: out}, nil
	}
	return nil, valueErr(driver.ErrUnsupported, fmt.Errorf("a %s value cannot be stored in DynamoDB", v.Type()))
}

// jsonAV converts a decoded JSON value (numbers as json.Number).
func jsonAV(x any, depth int) (types.AttributeValue, error) {
	if depth > maxDepth {
		return nil, valueErr(driver.ErrUnsupported, errors.New("the value nests deeper than DynamoDB's 32 levels"))
	}
	switch v := x.(type) {
	case nil:
		return &types.AttributeValueMemberNULL{Value: true}, nil
	case bool:
		return &types.AttributeValueMemberBOOL{Value: v}, nil
	case json.Number:
		return &types.AttributeValueMemberN{Value: v.String()}, nil
	case string:
		return &types.AttributeValueMemberS{Value: v}, nil
	case []any:
		out := make([]types.AttributeValue, len(v))
		for i, e := range v {
			av, err := jsonAV(e, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = av
		}
		return &types.AttributeValueMemberL{Value: out}, nil
	case map[string]any:
		out := make(map[string]types.AttributeValue, len(v))
		for k, e := range v {
			av, err := jsonAV(e, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = av
		}
		return &types.AttributeValueMemberM{Value: out}, nil
	}
	return nil, valueErr(driver.ErrSyntax, fmt.Errorf("unexpected JSON value %T", x))
}

func valueErr(kind, err error) error { return driver.Wrap(kind, Name, "value", err) }

// itemSize estimates the bytes an item holds: names plus values, as
// DynamoDB counts its 400 KB item limit.
func itemSize(item map[string]types.AttributeValue) int {
	n := 0
	for k, v := range item {
		n += len(k) + avSize(v)
	}
	return n
}

func avSize(av types.AttributeValue) int {
	switch v := av.(type) {
	case *types.AttributeValueMemberS:
		return len(v.Value)
	case *types.AttributeValueMemberN:
		return len(v.Value)
	case *types.AttributeValueMemberB:
		return len(v.Value)
	case *types.AttributeValueMemberM:
		return 3 + itemSize(v.Value)
	case *types.AttributeValueMemberL:
		n := 3
		for _, e := range v.Value {
			n += 1 + avSize(e)
		}
		return n
	case *types.AttributeValueMemberSS:
		n := 0
		for _, e := range v.Value {
			n += len(e)
		}
		return n
	case *types.AttributeValueMemberNS:
		n := 0
		for _, e := range v.Value {
			n += len(e)
		}
		return n
	case *types.AttributeValueMemberBS:
		n := 0
		for _, e := range v.Value {
			n += len(e)
		}
		return n
	}
	return 1
}
