package drivertest

import (
	"bytes"
	"encoding/json"
	"math"
	"math/big"
	"reflect"
	"slices"

	"github.com/xavidop/tobira/driver"
)

// EqualValue reports whether want and got carry the same meaning, ignoring
// representation differences between drivers:
//
//   - Int, Float and Decimal compare numerically and exactly (math/big), so
//     12.5 equals "12.50", while Float 0.1 does not equal "0.10".
//   - Bool equals Int 0 or 1.
//   - Time compares as an instant, truncated to whole seconds.
//   - JSON compares semantically, also against a String holding JSON and
//     against a List or Object (a document store's native form of it).
//   - Null equals only Null.
//   - List compares element-wise and Object by key (order-insensitive), both
//     recursively with EqualValue.
func EqualValue(want, got driver.Value) bool {
	wt, gt := want.Type(), got.Type()
	switch {
	case wt == driver.TypeNull || gt == driver.TypeNull:
		return wt == gt
	case isNumeric(wt) && isNumeric(gt):
		return equalNumeric(want, got)
	case wt == driver.TypeBool && gt == driver.TypeBool:
		a, _ := want.AsBool()
		b, _ := got.AsBool()
		return a == b
	case wt == driver.TypeBool && gt == driver.TypeInt:
		return boolEqualsInt(want, got)
	case wt == driver.TypeInt && gt == driver.TypeBool:
		return boolEqualsInt(got, want)
	case wt == driver.TypeTime && gt == driver.TypeTime:
		a, _ := want.AsTime()
		b, _ := got.AsTime()
		return a.Unix() == b.Unix()
	case wt == driver.TypeJSON || gt == driver.TypeJSON:
		return equalJSON(want, got)
	case wt == driver.TypeString && gt == driver.TypeString:
		a, _ := want.AsText()
		b, _ := got.AsText()
		return a == b
	case wt == driver.TypeBytes && gt == driver.TypeBytes:
		a, _ := want.AsBytes()
		b, _ := got.AsBytes()
		return bytes.Equal(a, b)
	case wt == driver.TypeVector && gt == driver.TypeVector:
		a, _ := want.AsVector()
		b, _ := got.AsVector()
		return slices.Equal(a, b)
	case wt == driver.TypeList && gt == driver.TypeList:
		a, _ := want.AsList()
		b, _ := got.AsList()
		return slices.EqualFunc(a, b, EqualValue)
	case wt == driver.TypeObject && gt == driver.TypeObject:
		a, _ := want.AsObject()
		b, _ := got.AsObject()
		return equalObject(a, b)
	case wt == driver.TypeUnknown && gt == driver.TypeUnknown:
		return want.String() == got.String()
	}
	return false
}

func isNumeric(t driver.Type) bool {
	return t == driver.TypeInt || t == driver.TypeFloat || t == driver.TypeDecimal
}

func boolEqualsInt(b, i driver.Value) bool {
	bv, _ := b.AsBool()
	iv, _ := i.AsInt()
	return (bv && iv == 1) || (!bv && iv == 0)
}

// toRat converts a numeric Value exactly. ok is false for NaN, Inf and
// unparseable decimals.
func toRat(v driver.Value) (*big.Rat, bool) {
	switch v.Type() {
	case driver.TypeInt:
		i, _ := v.AsInt()
		return new(big.Rat).SetInt64(i), true
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, false
		}
		return new(big.Rat).SetFloat64(f), true
	case driver.TypeDecimal:
		s, _ := v.AsText()
		return new(big.Rat).SetString(s)
	}
	return nil, false
}

func equalNumeric(a, b driver.Value) bool {
	ra, oka := toRat(a)
	rb, okb := toRat(b)
	if oka && okb {
		return ra.Cmp(rb) == 0
	}
	if a.Type() == driver.TypeFloat && b.Type() == driver.TypeFloat {
		fa, _ := a.AsFloat()
		fb, _ := b.AsFloat()
		return fa == fb || (math.IsNaN(fa) && math.IsNaN(fb))
	}
	if a.Type() == driver.TypeDecimal && b.Type() == driver.TypeDecimal {
		sa, _ := a.AsText()
		sb, _ := b.AsText()
		return sa == sb // unparseable (e.g. NaN) decimals
	}
	return false
}

func equalObject(a, b driver.Object) bool {
	if len(a) != len(b) {
		return false
	}
	for _, m := range a {
		o, ok := b.Get(m.Key)
		if !ok || !EqualValue(m.Value, o) {
			return false
		}
	}
	return true
}

// jsonText returns the JSON text of v: raw for JSON, the text for String,
// the encoding of a List or Object.
func jsonText(v driver.Value) ([]byte, bool) {
	if raw, ok := v.AsJSON(); ok {
		return raw, true
	}
	if s, ok := v.AsText(); ok && v.Type() == driver.TypeString {
		return []byte(s), true
	}
	if v.Type() == driver.TypeList || v.Type() == driver.TypeObject {
		b, err := v.MarshalJSON()
		return b, err == nil
	}
	return nil, false
}

func equalJSON(a, b driver.Value) bool {
	ra, oka := jsonText(a)
	rb, okb := jsonText(b)
	if !oka || !okb {
		return false
	}
	var xa, xb any
	if json.Unmarshal(ra, &xa) != nil || json.Unmarshal(rb, &xb) != nil {
		return false
	}
	return reflect.DeepEqual(xa, xb)
}
