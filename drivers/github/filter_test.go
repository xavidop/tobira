package github

import (
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestMatches(t *testing.T) {
	t.Parallel()
	t1 := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	tags := driver.List(driver.String("bug"), driver.String("help wanted"))
	cond := func(op driver.Op, v driver.Value) driver.Condition {
		return driver.Condition{Field: "f", Op: op, Value: v}
	}
	tests := []struct {
		name string
		v    driver.Value
		c    driver.Condition
		want bool
	}{
		// Numbers.
		{"int eq int", driver.Int(30), cond(driver.OpEq, driver.Int(30)), true},
		{"int eq float", driver.Int(30), cond(driver.OpEq, driver.Float(30)), true},
		{"int eq other", driver.Int(30), cond(driver.OpEq, driver.Int(31)), false},
		{"int ne", driver.Int(30), cond(driver.OpNe, driver.Int(31)), true},
		{"int ne same", driver.Int(30), cond(driver.OpNe, driver.Int(30)), false},
		{"int lt", driver.Int(30), cond(driver.OpLt, driver.Int(31)), true},
		{"int lt same", driver.Int(30), cond(driver.OpLt, driver.Int(30)), false},
		{"int le same", driver.Int(30), cond(driver.OpLe, driver.Int(30)), true},
		{"int gt", driver.Int(30), cond(driver.OpGt, driver.Int(30)), false},
		{"int gt smaller", driver.Int(30), cond(driver.OpGt, driver.Int(29)), true},
		{"int ge same", driver.Int(30), cond(driver.OpGe, driver.Int(30)), true},
		{"float ge", driver.Float(1.5), cond(driver.OpGe, driver.Float(1.5)), true},
		{"float lt int", driver.Float(1.5), cond(driver.OpLt, driver.Int(2)), true},
		{"float gt int", driver.Float(1.5), cond(driver.OpGt, driver.Int(2)), false},
		{"int eq numeric text", driver.Int(30), cond(driver.OpEq, driver.String("30")), true},
		{"int in", driver.Int(30), cond(driver.OpIn, driver.List(driver.Int(1), driver.Int(30))), true},
		{"int in floats", driver.Int(30), cond(driver.OpIn, driver.List(driver.Float(30))), true},
		{"int not in", driver.Int(30), cond(driver.OpIn, driver.List(driver.Int(1), driver.Int(2))), false},
		{"int in empty", driver.Int(30), cond(driver.OpIn, driver.List()), false},
		// Strings.
		{"string eq", driver.String("open"), cond(driver.OpEq, driver.String("open")), true},
		{"string eq case", driver.String("open"), cond(driver.OpEq, driver.String("Open")), false},
		{"string ne", driver.String("open"), cond(driver.OpNe, driver.String("closed")), true},
		{"string lt", driver.String("apple"), cond(driver.OpLt, driver.String("banana")), true},
		{"string ge", driver.String("banana"), cond(driver.OpGe, driver.String("apple")), true},
		{"string contains", driver.String("Ada Lovelace"), cond(driver.OpContains, driver.String("Love")), true},
		{"string contains missing", driver.String("Ada Lovelace"), cond(driver.OpContains, driver.String("Byron")), false},
		{"string prefix", driver.String("Ada Lovelace"), cond(driver.OpPrefix, driver.String("Ada")), true},
		{"string prefix inner", driver.String("Ada Lovelace"), cond(driver.OpPrefix, driver.String("Love")), false},
		{"string in", driver.String("open"), cond(driver.OpIn, driver.List(driver.String("closed"), driver.String("open"))), true},
		{"string eq decimal text", driver.Decimal("12.50"), cond(driver.OpEq, driver.String("12.50")), true},
		// Booleans.
		{"bool eq", driver.Bool(true), cond(driver.OpEq, driver.Bool(true)), true},
		{"bool eq other", driver.Bool(true), cond(driver.OpEq, driver.Bool(false)), false},
		{"bool ne", driver.Bool(true), cond(driver.OpNe, driver.Bool(false)), true},
		{"false lt true", driver.Bool(false), cond(driver.OpLt, driver.Bool(true)), true},
		{"true gt false", driver.Bool(true), cond(driver.OpGt, driver.Bool(false)), true},
		{"bool eq text", driver.Bool(true), cond(driver.OpEq, driver.String("true")), true},
		{"bool eq other text", driver.Bool(true), cond(driver.OpEq, driver.String("false")), false},
		{"bool in", driver.Bool(false), cond(driver.OpIn, driver.List(driver.Bool(false))), true},
		// Times, against times and against RFC 3339 text.
		{"time eq time", driver.Time(t1), cond(driver.OpEq, driver.Time(t1)), true},
		{"time eq other zone", driver.Time(t1), cond(driver.OpEq, driver.Time(t1.In(time.FixedZone("x", 3600)))), true},
		{"time eq text", driver.Time(t1), cond(driver.OpEq, driver.String("2024-03-04T05:06:07Z")), true},
		{"time eq text other zone", driver.Time(t1), cond(driver.OpEq, driver.String("2024-03-04T07:06:07+02:00")), true},
		{"time lt text", driver.Time(t1), cond(driver.OpLt, driver.String("2024-03-04T06:06:07Z")), true},
		{"time ge time", driver.Time(t2), cond(driver.OpGe, driver.Time(t1)), true},
		{"time le time", driver.Time(t2), cond(driver.OpLe, driver.Time(t1)), false},
		{"time gt text", driver.Time(t2), cond(driver.OpGt, driver.String("2024-03-04T05:06:07Z")), true},
		{"time ne text", driver.Time(t2), cond(driver.OpNe, driver.String("2024-03-04T05:06:07Z")), true},
		{"text eq time", driver.String("2024-03-04T05:06:07Z"), cond(driver.OpEq, driver.Time(t1)), true},
		{"time in", driver.Time(t1), cond(driver.OpIn, driver.List(driver.String("2024-03-04T05:06:07Z"))), true},
		{"time eq non-time text", driver.Time(t1), cond(driver.OpEq, driver.String("yesterday")), false},
		// Lists.
		{"list contains member", tags, cond(driver.OpContains, driver.String("help wanted")), true},
		{"list contains other", tags, cond(driver.OpContains, driver.String("help")), false},
		{"list contains number", driver.List(driver.Int(1), driver.Int(2)), cond(driver.OpContains, driver.Int(2)), true},
		{"empty list contains", driver.List(), cond(driver.OpContains, driver.String("bug")), false},
		{"list eq", tags, cond(driver.OpEq, driver.String("bug")), false},
		{"list in", tags, cond(driver.OpIn, driver.List(driver.String("bug"))), false},
		{"list prefix", tags, cond(driver.OpPrefix, driver.String("bug")), false},
		// Nulls.
		{"null is_null", driver.Null(), cond(driver.OpIsNull, driver.Null()), true},
		{"value is_null", driver.Int(1), cond(driver.OpIsNull, driver.Null()), false},
		{"null not_null", driver.Null(), cond(driver.OpNotNull, driver.Null()), false},
		{"value not_null", driver.Int(1), cond(driver.OpNotNull, driver.Null()), true},
		{"null eq", driver.Null(), cond(driver.OpEq, driver.Int(1)), false},
		{"null ne", driver.Null(), cond(driver.OpNe, driver.Int(1)), false},
		{"null in", driver.Null(), cond(driver.OpIn, driver.List(driver.Null())), false},
		{"null contains", driver.Null(), cond(driver.OpContains, driver.String("")), false},
		{"value eq null", driver.Int(1), cond(driver.OpEq, driver.Null()), false},
		// Incompatible types compare as no match.
		{"time eq int", driver.Time(t1), cond(driver.OpEq, driver.Int(1)), false},
		{"time ne int", driver.Time(t1), cond(driver.OpNe, driver.Int(1)), false},
		{"int lt list", driver.Int(1), cond(driver.OpLt, driver.List(driver.Int(2))), false},
		{"bool contains", driver.Bool(true), cond(driver.OpContains, driver.String("t")), false},
		{"time prefix", driver.Time(t1), cond(driver.OpPrefix, driver.String("2024")), false},
		{"json eq", driver.JSON([]byte(`{"a":1}`)), cond(driver.OpEq, driver.String(`{"a":1}`)), false},
		{"string lt int", driver.String("a"), cond(driver.OpLt, driver.Float(1.5)), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := matches(tc.v, tc.c)
			if err != nil {
				t.Fatalf("matches: %v", err)
			}
			if got != tc.want {
				t.Errorf("matches(%v, %s %v) = %v, want %v", tc.v, tc.c.Op, tc.c.Value, got, tc.want)
			}
		})
	}
}

func TestMatchesErrors(t *testing.T) {
	t.Parallel()
	// in without a list is refused; an unknown operator too.
	if _, err := matches(driver.Int(1), driver.Condition{Field: "f", Op: driver.OpIn, Value: driver.Int(1)}); err == nil || !strings.Contains(err.Error(), "in takes a list") {
		t.Errorf("in with a scalar = %v", err)
	}
	if _, err := matches(driver.Int(1), driver.Condition{Field: "f", Op: "regex", Value: driver.String("1")}); err == nil || !strings.Contains(err.Error(), `operator "regex"`) {
		t.Errorf("unknown operator = %v", err)
	}
	// A null value short-circuits before the operator is checked.
	if ok, err := matches(driver.Null(), driver.Condition{Field: "f", Op: "regex", Value: driver.String("1")}); err != nil || ok {
		t.Errorf("unknown operator on null = %v, %v", ok, err)
	}
}

func TestMatchesAll(t *testing.T) {
	t.Parallel()
	cols := []column{{name: "number"}, {name: "state"}, {name: "labels"}}
	row := driver.Row{driver.Int(7), driver.String("open"), driver.List(driver.String("bug"))}
	cond := func(f string, op driver.Op, v driver.Value) driver.Condition {
		return driver.Condition{Field: f, Op: op, Value: v}
	}
	if ok, err := matchesAll(cols, row, nil); err != nil || !ok {
		t.Errorf("no conditions = %v, %v", ok, err)
	}
	if ok, err := matchesAll(cols, row, []driver.Condition{cond("number", driver.OpGt, driver.Int(1)), cond("state", driver.OpEq, driver.String("open")), cond("labels", driver.OpContains, driver.String("bug"))}); err != nil || !ok {
		t.Errorf("all matching = %v, %v", ok, err)
	}
	if ok, err := matchesAll(cols, row, []driver.Condition{cond("number", driver.OpGt, driver.Int(1)), cond("state", driver.OpEq, driver.String("closed"))}); err != nil || ok {
		t.Errorf("one failing = %v, %v", ok, err)
	}
	if _, err := matchesAll(cols, row, []driver.Condition{cond("title", driver.OpEq, driver.String("x"))}); err == nil || !strings.Contains(err.Error(), `unknown field "title"`) {
		t.Errorf("unknown field = %v", err)
	}
	if _, err := matchesAll(cols, row, []driver.Condition{cond("number", driver.OpIn, driver.Int(7))}); err == nil {
		t.Error("a bad condition should fail the row")
	}
}

func TestCompareValues(t *testing.T) {
	t.Parallel()
	t1 := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	tests := []struct {
		name string
		a, b driver.Value
		want int
		ok   bool
	}{
		{"ints", driver.Int(1), driver.Int(2), -1, true},
		{"int and float", driver.Int(2), driver.Float(1.5), 1, true},
		{"floats equal", driver.Float(1.5), driver.Float(1.5), 0, true},
		{"int and numeric text", driver.Int(30), driver.String("30"), 0, true},
		{"int and decimal text", driver.Int(30), driver.Decimal("30"), 0, true},
		{"decimal and float", driver.Decimal("12.50"), driver.Float(12.5), 0, false},
		{"strings", driver.String("a"), driver.String("b"), -1, true},
		{"strings equal", driver.String("a"), driver.String("a"), 0, true},
		{"bools equal", driver.Bool(true), driver.Bool(true), 0, true},
		{"false before true", driver.Bool(false), driver.Bool(true), -1, true},
		{"true after false", driver.Bool(true), driver.Bool(false), 1, true},
		{"bool and its text", driver.Bool(true), driver.String("true"), 0, true},
		{"bool and other text", driver.Bool(false), driver.String("true"), -1, true},
		{"bool and int", driver.Bool(true), driver.Int(1), 1, true},
		{"bool and time", driver.Bool(true), driver.Time(t1), 0, false},
		{"times", driver.Time(t1), driver.Time(t1.Add(time.Second)), -1, true},
		{"time and text", driver.Time(t1), driver.String("2024-03-04T05:06:07Z"), 0, true},
		{"text and time", driver.String("2024-03-04T05:06:08Z"), driver.Time(t1), 1, true},
		{"time and non-time text", driver.Time(t1), driver.String("later"), 0, false},
		{"time and int", driver.Time(t1), driver.Int(1), 0, false},
		{"null and int", driver.Null(), driver.Int(1), 0, false},
		{"int and null", driver.Int(1), driver.Null(), 0, false},
		{"nulls", driver.Null(), driver.Null(), 0, false},
		{"lists", driver.List(driver.Int(1)), driver.List(driver.Int(1)), 0, false},
		{"json and text", driver.JSON([]byte(`1`)), driver.String("1"), 0, false},
		{"bytes and text", driver.Bytes([]byte("a")), driver.String("a"), 0, false},
		{"string and int", driver.String("a"), driver.Int(1), 1, true},
	}
	for _, tc := range tests {
		got, ok := compareValues(tc.a, tc.b)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("%s: compareValues(%v, %v) = %d, %v; want %d, %v", tc.name, tc.a, tc.b, got, ok, tc.want, tc.ok)
		}
	}
}
