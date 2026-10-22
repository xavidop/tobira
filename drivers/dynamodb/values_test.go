package dynamodb

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestValueOf(t *testing.T) {
	for _, tc := range []struct {
		name   string
		av     types.AttributeValue
		want   driver.Value
		native string
	}{
		{"string", &types.AttributeValueMemberS{Value: "héllo"}, driver.String("héllo"), "S"},
		{"int", &types.AttributeValueMemberN{Value: "42"}, driver.Int(42), "N"},
		{"negative int", &types.AttributeValueMemberN{Value: "-7"}, driver.Int(-7), "N"},
		{"decimal", &types.AttributeValueMemberN{Value: "12.5"}, driver.Decimal("12.5"), "N"},
		{"exponent whole", &types.AttributeValueMemberN{Value: "1E+2"}, driver.Int(100), "N"},
		{"big int", &types.AttributeValueMemberN{Value: "123456789012345678901234567890"}, driver.Decimal("123456789012345678901234567890"), "N"},
		{"bytes", &types.AttributeValueMemberB{Value: []byte{0, 1, 2}}, driver.Bytes([]byte{0, 1, 2}), "B"},
		{"bool", &types.AttributeValueMemberBOOL{Value: true}, driver.Bool(true), "BOOL"},
		{"null", &types.AttributeValueMemberNULL{Value: true}, driver.Null(), "NULL"},
		{"list", &types.AttributeValueMemberL{Value: []types.AttributeValue{
			&types.AttributeValueMemberS{Value: "a"}, &types.AttributeValueMemberN{Value: "1"},
		}}, driver.List(driver.String("a"), driver.Int(1)), "L"},
		{"empty list", &types.AttributeValueMemberL{}, driver.List(), "L"},
		{"map keys sorted", &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
			"z": &types.AttributeValueMemberBOOL{Value: false}, "a": &types.AttributeValueMemberS{Value: "x"},
		}}, driver.ObjectValue(driver.Member{Key: "a", Value: driver.String("x")}, driver.Member{Key: "z", Value: driver.Bool(false)}), "M"},
		{"string set sorted", &types.AttributeValueMemberSS{Value: []string{"b", "a"}}, driver.List(driver.String("a"), driver.String("b")), "SS"},
		{"number set sorted numerically", &types.AttributeValueMemberNS{Value: []string{"10", "9", "1.5"}}, driver.List(driver.Decimal("1.5"), driver.Int(9), driver.Int(10)), "NS"},
		{"binary set", &types.AttributeValueMemberBS{Value: [][]byte{{2}, {1}}}, driver.List(driver.Bytes([]byte{1}), driver.Bytes([]byte{2})), "BS"},
		{"nil", nil, driver.Null(), ""},
	} {
		got := valueOf(tc.av)
		if got.Type() != tc.want.Type() || !drivertest.EqualValue(tc.want, got) {
			t.Errorf("%s: got %s (%s), want %s (%s)", tc.name, got, got.Type(), tc.want, tc.want.Type())
		}
		if n := nativeType(tc.av); n != tc.native {
			t.Errorf("%s: native type %q, want %q", tc.name, n, tc.native)
		}
	}
}

func TestToAV(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 600, time.FixedZone("x", 3600))
	for _, tc := range []struct {
		name string
		in   driver.Value
		want types.AttributeValue
	}{
		{"null", driver.Null(), &types.AttributeValueMemberNULL{Value: true}},
		{"bool", driver.Bool(true), &types.AttributeValueMemberBOOL{Value: true}},
		{"int", driver.Int(-3), &types.AttributeValueMemberN{Value: "-3"}},
		{"float", driver.Float(1.25), &types.AttributeValueMemberN{Value: "1.25"}},
		{"decimal", driver.Decimal("12.50"), &types.AttributeValueMemberN{Value: "12.50"}},
		{"string", driver.String("x"), &types.AttributeValueMemberS{Value: "x"}},
		{"bytes", driver.Bytes([]byte{9}), &types.AttributeValueMemberB{Value: []byte{9}}},
		{"time is RFC 3339 UTC", driver.Time(ts), &types.AttributeValueMemberS{Value: "2026-01-02T02:04:05.0000006Z"}},
		{"json array", driver.JSON([]byte(`["a",1,2.5,true,null,{"k":"v"}]`)), &types.AttributeValueMemberL{Value: []types.AttributeValue{
			&types.AttributeValueMemberS{Value: "a"}, &types.AttributeValueMemberN{Value: "1"}, &types.AttributeValueMemberN{Value: "2.5"},
			&types.AttributeValueMemberBOOL{Value: true}, &types.AttributeValueMemberNULL{Value: true},
			&types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"k": &types.AttributeValueMemberS{Value: "v"}}},
		}}},
		{"empty json array", driver.JSON([]byte(`[]`)), &types.AttributeValueMemberL{Value: []types.AttributeValue{}}},
		{"vector", driver.Vector([]float32{0.5, 1}), &types.AttributeValueMemberL{Value: []types.AttributeValue{
			&types.AttributeValueMemberN{Value: "0.5"}, &types.AttributeValueMemberN{Value: "1"},
		}}},
		{"list", driver.List(driver.Int(1)), &types.AttributeValueMemberL{Value: []types.AttributeValue{&types.AttributeValueMemberN{Value: "1"}}}},
		{"object", driver.ObjectValue(driver.Member{Key: "a", Value: driver.Int(1)}), &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"a": &types.AttributeValueMemberN{Value: "1"}}}},
	} {
		got, err := toAV(tc.in)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !drivertest.EqualValue(valueOf(tc.want), valueOf(got)) || nativeType(got) != nativeType(tc.want) {
			t.Errorf("%s: got %#v, want %#v", tc.name, got, tc.want)
		}
		if n, ok := got.(*types.AttributeValueMemberN); ok && n.Value != tc.want.(*types.AttributeValueMemberN).Value {
			t.Errorf("%s: number text %q, want %q", tc.name, n.Value, tc.want.(*types.AttributeValueMemberN).Value)
		}
		if s, ok := got.(*types.AttributeValueMemberS); ok && s.Value != tc.want.(*types.AttributeValueMemberS).Value {
			t.Errorf("%s: string %q, want %q", tc.name, s.Value, tc.want.(*types.AttributeValueMemberS).Value)
		}
	}
}

func TestToAVRefuses(t *testing.T) {
	for name, v := range map[string]driver.Value{
		"NaN":          driver.Float(math.NaN()),
		"Inf":          driver.Float(math.Inf(1)),
		"bad decimal":  driver.Decimal("12,5"),
		"bad json":     driver.JSON([]byte(`{`)),
		"unknown type": driver.Unknown("x", []byte{1}, "x"),
	} {
		_, err := toAV(v)
		var de *driver.Error
		if err == nil || !errors.As(err, &de) || de.Driver != Name {
			t.Errorf("%s: want a *driver.Error, got %v", name, err)
		}
	}
}

func TestToAVDepthLimit(t *testing.T) {
	v := driver.Int(1)
	for range maxDepth + 1 {
		v = driver.List(v)
	}
	if _, err := toAV(v); err == nil {
		t.Error("a value nested deeper than DynamoDB allows was accepted")
	}
}

func TestItemSize(t *testing.T) {
	item := map[string]types.AttributeValue{
		"id":   &types.AttributeValueMemberN{Value: "12"},
		"name": &types.AttributeValueMemberS{Value: "abcd"},
		"l":    &types.AttributeValueMemberL{Value: []types.AttributeValue{&types.AttributeValueMemberS{Value: "xy"}}},
	}
	if got := itemSize(item); got < 2+2+4+4+1+2 {
		t.Errorf("itemSize = %d, too small", got)
	}
}
