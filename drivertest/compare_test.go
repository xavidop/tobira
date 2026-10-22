package drivertest

import (
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestEqualValue(t *testing.T) {
	inst := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	est := time.FixedZone("X", -5*3600)
	j := func(s string) driver.Value { return driver.JSON([]byte(s)) }
	tests := []struct {
		name      string
		want, got driver.Value
		eq        bool
	}{
		{"int bool true", driver.Int(1), driver.Bool(true), true},
		{"bool int false", driver.Bool(false), driver.Int(0), true},
		{"int 2 vs bool", driver.Int(2), driver.Bool(true), false},
		{"float decimal", driver.Float(12.5), driver.Decimal("12.50"), true},
		{"decimal decimal", driver.Decimal("12.5"), driver.Decimal("12.50"), true},
		{"decimal differs", driver.Decimal("12.51"), driver.Decimal("12.50"), false},
		{"decimal exact big", driver.Decimal("12345678901234567890.01"), driver.Decimal("12345678901234567890.02"), false},
		{"float inexact decimal", driver.Float(0.1), driver.Decimal("0.10"), false},
		{"float vs int", driver.Float(12), driver.Int(12), true},
		{"float 12.5 vs int 12", driver.Float(12.5), driver.Int(12), false},
		{"decimal vs int 12", driver.Decimal("12.50"), driver.Int(12), false},
		{"decimal int", driver.Decimal("12.00"), driver.Int(12), true},
		{"time zones", driver.Time(inst), driver.Time(inst.In(est)), true},
		{"time sub-second", driver.Time(inst), driver.Time(inst.Add(500 * time.Millisecond)), true},
		{"time differs", driver.Time(inst), driver.Time(inst.Add(time.Second)), false},
		{"json order", j(`{"a":1,"b":2}`), j(`{"b":2,"a":1}`), true},
		{"json vs string", j(`{"a":1,"b":2}`), driver.String(`{"a":1,"b":2}`), true},
		{"string vs json", driver.String(`{"a":1}`), j(`{"a": 1}`), true},
		{"json differs", j(`[1]`), j(`[2]`), false},
		{"json invalid", j(`{`), j(`{`), false},
		{"string vs string no json", driver.String("a"), driver.String("a"), true},
		{"string differs", driver.String("a"), driver.String("b"), false},
		{"null null", driver.Null(), driver.Null(), true},
		{"null vs empty", driver.Null(), driver.String(""), false},
		{"empty vs null", driver.String(""), driver.Null(), false},
		{"null vs zero", driver.Null(), driver.Int(0), false},
		{"string vs int", driver.String("1"), driver.Int(1), false},
		{"bytes", driver.Bytes([]byte{1, 2}), driver.Bytes([]byte{1, 2}), true},
		{"list", driver.List(driver.Int(1), driver.Decimal("2.0")), driver.List(driver.Float(1), driver.Int(2)), true},
		{"list len", driver.List(driver.Int(1)), driver.List(driver.Int(1), driver.Int(2)), false},
		{"object order",
			driver.ObjectValue(driver.Member{Key: "a", Value: driver.Int(1)}, driver.Member{Key: "b", Value: driver.Bool(true)}),
			driver.ObjectValue(driver.Member{Key: "b", Value: driver.Int(1)}, driver.Member{Key: "a", Value: driver.Float(1)}), true},
		{"object key differs",
			driver.ObjectValue(driver.Member{Key: "a", Value: driver.Int(1)}),
			driver.ObjectValue(driver.Member{Key: "b", Value: driver.Int(1)}), false},
		{"vector", driver.Vector([]float32{1, 2}), driver.Vector([]float32{1, 2}), true},
		{"vector differs", driver.Vector([]float32{1, 2}), driver.Vector([]float32{1, 3}), false},
		// A document store returns a JSON fixture value as a native List or Object.
		{"json vs list", j(`["admin","beta"]`), driver.List(driver.String("admin"), driver.String("beta")), true},
		{"json vs empty list", j(`[]`), driver.List(), true},
		{"json vs list differs", j(`["admin"]`), driver.List(driver.String("beta")), false},
		{"json vs object", j(`{"a":1,"b":[true]}`), driver.ObjectValue(driver.Member{Key: "b", Value: driver.List(driver.Bool(true))}, driver.Member{Key: "a", Value: driver.Int(1)}), true},
		{"list vs json", driver.List(driver.Int(1)), j(`[1]`), true},
		{"json vs int", j(`1`), driver.Int(1), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := EqualValue(tc.want, tc.got); got != tc.eq {
				t.Errorf("EqualValue(%v, %v) = %v, want %v", tc.want, tc.got, got, tc.eq)
			}
		})
	}
}
