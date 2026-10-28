package github

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

// assertValue compares two values: times by instant and location name, the
// rest structurally.
func assertValue(t *testing.T, what string, got, want driver.Value) {
	t.Helper()
	if got.Type() != want.Type() {
		t.Errorf("%s = %v (%s), want %v (%s)", what, got, got.Type(), want, want.Type())
		return
	}
	if want.Type() == driver.TypeTime {
		gt, _ := got.AsTime()
		wt, _ := want.AsTime()
		if !gt.Equal(wt) || gt.Location().String() != wt.Location().String() {
			t.Errorf("%s = %v in %s, want %v in %s", what, gt, gt.Location(), wt, wt.Location())
		}
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestTypedValue(t *testing.T) {
	t.Parallel()
	utc := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		raw  string
		typ  driver.Type
		want driver.Value
	}{
		{`"a"`, driver.TypeString, driver.String("a")},
		{`""`, driver.TypeString, driver.String("")},
		{`1`, driver.TypeString, driver.JSON([]byte(`1`))},
		{`["a"]`, driver.TypeString, driver.JSON([]byte(`["a"]`))},
		{`12`, driver.TypeInt, driver.Int(12)},
		{`-12`, driver.TypeInt, driver.Int(-12)},
		{`1.5`, driver.TypeInt, driver.Float(1.5)},
		{`99999999999999999999`, driver.TypeInt, driver.Decimal("99999999999999999999")},
		{`"12"`, driver.TypeInt, driver.Int(12)}, // a quoted number still reads as a number
		{`"abc"`, driver.TypeInt, driver.JSON([]byte(`"abc"`))},
		{`1.5`, driver.TypeFloat, driver.Float(1.5)},
		{`2`, driver.TypeFloat, driver.Int(2)},
		{`true`, driver.TypeBool, driver.Bool(true)},
		{`false`, driver.TypeBool, driver.Bool(false)},
		{`"true"`, driver.TypeBool, driver.JSON([]byte(`"true"`))},
		{`1`, driver.TypeBool, driver.JSON([]byte(`1`))},
		{`"2024-01-02T03:04:05Z"`, driver.TypeTime, driver.Time(utc)},
		{`"2024-01-02T05:04:05+02:00"`, driver.TypeTime, driver.Time(utc)},
		{`"2024-01-02T03:04:05.123456789Z"`, driver.TypeTime, driver.Time(utc.Add(123456789))},
		{`"yesterday"`, driver.TypeTime, driver.String("yesterday")},
		{`"2024-01-02"`, driver.TypeTime, driver.String("2024-01-02")},
		{`1704164645`, driver.TypeTime, driver.JSON([]byte(`1704164645`))},
		{`[1,"a",true,null,{"x":1},[2]]`, driver.TypeList, driver.List(driver.Int(1), driver.String("a"), driver.Bool(true), driver.Null(), driver.JSON([]byte(`{"x":1}`)), driver.JSON([]byte(`[2]`)))},
		{`[]`, driver.TypeList, driver.List()},
		{`"x"`, driver.TypeList, driver.JSON([]byte(`"x"`))},
		{`{}`, driver.TypeList, driver.JSON([]byte(`{}`))},
		{`"x"`, driver.TypeBytes, driver.JSON([]byte(`"x"`))},
		{`{"a":1}`, driver.TypeJSON, driver.JSON([]byte(`{"a":1}`))},
		{`{"a":1}`, driver.TypeObject, driver.JSON([]byte(`{"a":1}`))},
		{`null`, driver.TypeString, driver.Null()},
		{`null`, driver.TypeInt, driver.Null()},
		{`null`, driver.TypeTime, driver.Null()},
		{`null`, driver.TypeList, driver.Null()},
		{`  null `, driver.TypeBool, driver.Null()},
		{``, driver.TypeString, driver.Null()},
	}
	for _, tc := range tests {
		assertValue(t, "typedValue("+tc.raw+", "+tc.typ.String()+")", typedValue(json.RawMessage(tc.raw), tc.typ), tc.want)
	}
}

func TestAnyValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		raw  string
		want driver.Value
	}{
		{`true`, driver.Bool(true)},
		{`false`, driver.Bool(false)},
		{`12`, driver.Int(12)},
		{`-3`, driver.Int(-3)},
		{`0`, driver.Int(0)},
		{`99999999999999999999`, driver.Decimal("99999999999999999999")},
		{`-99999999999999999999`, driver.Decimal("-99999999999999999999")},
		{`1.5`, driver.Float(1.5)},
		{`1e3`, driver.Float(1000)},
		{`-2.5E-1`, driver.Float(-0.25)},
		{`"s"`, driver.String("s")},
		{`""`, driver.String("")},
		{`"café"`, driver.String("café")},
		{`{"a":1}`, driver.JSON([]byte(`{"a":1}`))},
		{`[1]`, driver.JSON([]byte(`[1]`))},
		{`[]`, driver.JSON([]byte(`[]`))},
		{`null`, driver.Null()},
		{``, driver.Null()},
		{`   `, driver.Null()},
		{`  12 `, driver.Int(12)},
		{` "s" `, driver.String("s")},
		{`tru`, driver.JSON([]byte(`tru`))},
		{`-`, driver.JSON([]byte(`-`))},
		{`12abc`, driver.JSON([]byte(`12abc`))},
		{`"unterminated`, driver.JSON([]byte(`"unterminated`))},
	}
	for _, tc := range tests {
		assertValue(t, "anyValue("+tc.raw+")", anyValue(json.RawMessage(tc.raw)), tc.want)
	}
}

func TestNumberValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		n    string
		want driver.Value
	}{
		{"12", driver.Int(12)},
		{"-0", driver.Int(0)},
		{"9223372036854775807", driver.Int(math.MaxInt64)},
		{"-9223372036854775808", driver.Int(math.MinInt64)},
		{"9223372036854775808", driver.Decimal("9223372036854775808")},
		{"-9223372036854775809", driver.Decimal("-9223372036854775809")},
		{"1.5", driver.Float(1.5)},
		{"0.1", driver.Float(0.1)},
		{"2E2", driver.Float(200)},
		{"1e-7", driver.Float(1e-7)},
		{"1.0", driver.Float(1)},
		{"1e400", driver.Decimal("1e400")},
		{"-1e400", driver.Decimal("-1e400")},
	}
	for _, tc := range tests {
		assertValue(t, "numberValue("+tc.n+")", numberValue(json.Number(tc.n)), tc.want)
	}
}

func TestToJSON(t *testing.T) {
	t.Parallel()
	t1 := time.Date(2024, 1, 2, 5, 4, 5, 0, time.FixedZone("x", 2*3600))
	tests := []struct {
		name string
		v    driver.Value
		want any
	}{
		{"null", driver.Null(), nil},
		{"bool", driver.Bool(true), true},
		{"int", driver.Int(5), int64(5)},
		{"float", driver.Float(1.5), 1.5},
		{"decimal", driver.Decimal("12.50"), json.Number("12.50")},
		{"string", driver.String("s"), "s"},
		{"time in UTC", driver.Time(t1), "2024-01-02T03:04:05Z"},
		{"json", driver.JSON([]byte(`{"a": 1}`)), json.RawMessage(`{"a": 1}`)},
		{"list", driver.List(driver.Int(1), driver.String("a"), driver.Null()), []any{int64(1), "a", nil}},
		{"empty list", driver.List(), []any{}},
		{"object", driver.ObjectValue(driver.Member{Key: "a", Value: driver.Int(1)}, driver.Member{Key: "b", Value: driver.List(driver.Bool(false))}), map[string]any{"a": int64(1), "b": []any{false}}},
	}
	for _, tc := range tests {
		got, err := toJSON(tc.v)
		if err != nil {
			t.Errorf("%s: toJSON: %v", tc.name, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: toJSON = %#v, want %#v", tc.name, got, tc.want)
		}
		// The result must be what encoding/json can send.
		if _, err := json.Marshal(got); err != nil {
			t.Errorf("%s: json.Marshal: %v", tc.name, err)
		}
	}
	refused := []struct {
		name string
		v    driver.Value
		hint string
	}{
		{"bytes", driver.Bytes([]byte{1}), "bytes value has no GitHub spelling"},
		{"vector", driver.Vector([]float32{1}), "vector value has no GitHub spelling"},
		{"unknown", driver.Unknown("geometry", []byte{1}, "POINT"), "unknown value has no GitHub spelling"},
		{"NaN", driver.Float(math.NaN()), "NaN and infinities"},
		{"+Inf", driver.Float(math.Inf(1)), "NaN and infinities"},
		{"-Inf", driver.Float(math.Inf(-1)), "NaN and infinities"},
		{"invalid json", driver.JSON([]byte(`{bad`)), "invalid JSON"},
		{"list with bytes", driver.List(driver.Int(1), driver.Bytes([]byte{1})), "bytes value"},
		{"object with NaN", driver.ObjectValue(driver.Member{Key: "a", Value: driver.Float(math.NaN())}), "NaN and infinities"},
	}
	for _, tc := range refused {
		got, err := toJSON(tc.v)
		if err == nil {
			t.Errorf("%s: toJSON = %#v, want an error", tc.name, got)
			continue
		}
		if !strings.Contains(err.Error(), tc.hint) {
			t.Errorf("%s: toJSON = %v, want %q", tc.name, err, tc.hint)
		}
	}
}

func TestStringList(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		v    driver.Value
		want []string
	}{
		{"list", driver.List(driver.String("a"), driver.String("b")), []string{"a", "b"}},
		{"list of decimals", driver.List(driver.Decimal("1")), []string{"1"}},
		{"empty list", driver.List(), []string{}},
		{"json array", driver.JSON([]byte(`["a", "b"]`)), []string{"a", "b"}},
		{"empty json array", driver.JSON([]byte(`[]`)), []string{}},
		{"json null", driver.JSON([]byte(`null`)), nil},
		{"comma text", driver.String("a, b ,c"), []string{"a", "b", "c"}},
		{"one name", driver.String("bug"), []string{"bug"}},
		{"empty text", driver.String(""), []string{}},
		{"blank text", driver.String("  "), []string{}},
		{"null", driver.Null(), []string{}},
	}
	for _, tc := range tests {
		got, err := stringList(tc.v)
		if err != nil {
			t.Errorf("%s: stringList: %v", tc.name, err)
			continue
		}
		if len(got) != len(tc.want) || (len(got) > 0 && !reflect.DeepEqual(got, tc.want)) {
			t.Errorf("%s: stringList = %q, want %q", tc.name, got, tc.want)
		}
	}
	for _, v := range []driver.Value{
		driver.List(driver.Int(1)),
		driver.List(driver.String("a"), driver.Bool(true)),
		driver.JSON([]byte(`[1]`)),
		driver.JSON([]byte(`"x"`)),
		driver.JSON([]byte(`{"a":"b"}`)),
		driver.JSON([]byte(`[`)),
		driver.Int(1),
		driver.Bool(true),
		driver.Time(time.Now()),
		driver.Bytes([]byte("a")),
	} {
		if got, err := stringList(v); err == nil {
			t.Errorf("stringList(%v) = %q, want an error", v, got)
		}
	}
}

func TestText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		v    driver.Value
		want string
		ok   bool
	}{
		{driver.String("a"), "a", true},
		{driver.String(""), "", true},
		{driver.Decimal("12.50"), "12.50", true},
		{driver.Int(12), "12", true},
		{driver.Int(-1), "-1", true},
		{driver.Float(1.5), "", false},
		{driver.Bool(true), "", false},
		{driver.Time(time.Now()), "", false},
		{driver.Null(), "", false},
		{driver.List(driver.String("a")), "", false},
		{driver.JSON([]byte(`"a"`)), "", false},
	} {
		got, ok := text(tc.v)
		if got != tc.want || ok != tc.ok {
			t.Errorf("text(%v) = %q, %v; want %q, %v", tc.v, got, ok, tc.want, tc.ok)
		}
	}
}

func TestMember(t *testing.T) {
	t.Parallel()
	obj, err := objectOf(json.RawMessage(`{
		"sha": "abc",
		"commit": {"author": {"name": "A", "date": "2024-01-01T00:00:00Z"}, "message": "m", "tree": null},
		"author": null,
		"labels": [{"name": "bug"}],
		"empty": {}
	}`))
	if err != nil {
		t.Fatalf("objectOf: %v", err)
	}
	found := map[string]string{
		"sha":                "\"abc\"",
		"commit.message":     "\"m\"",
		"commit.author.name": "\"A\"",
		"commit.author.date": "\"2024-01-01T00:00:00Z\"",
		"labels":             `[{"name": "bug"}]`,
		"empty":              "{}",
	}
	for path, want := range found {
		raw, ok := member(obj, path)
		if !ok || string(raw) != want {
			t.Errorf("member(%q) = %s, %v; want %s", path, raw, ok, want)
		}
	}
	for _, path := range []string{
		"missing",
		"commit.missing",
		"commit.author.missing",
		"commit.author.name.deeper", // through a string
		"commit.tree",               // null
		"commit.tree.sha",           // through a null
		"author",                    // null at the top
		"author.login",              // through a null at the top
		"labels.name",               // through an array
		"sha.x",                     // through a string
		"",
		"commit.",
		".sha",
	} {
		if raw, ok := member(obj, path); ok {
			t.Errorf("member(%q) = %s, want absent", path, raw)
		}
	}
}

func TestObjectOf(t *testing.T) {
	t.Parallel()
	obj, err := objectOf(json.RawMessage(`{"a": 1, "b": {"c": null}}`))
	if err != nil || len(obj) != 2 || string(obj["a"]) != "1" || string(obj["b"]) != `{"c": null}` {
		t.Errorf("objectOf = %v, %v", obj, err)
	}
	obj, err = objectOf(json.RawMessage(`{}`))
	if err != nil || obj == nil || len(obj) != 0 {
		t.Errorf("empty object = %v, %v", obj, err)
	}
	for raw, hint := range map[string]string{
		`null`:     "is null",
		`[1]`:      "not a JSON object",
		`"s"`:      "not a JSON object",
		`1`:        "not a JSON object",
		`{bad`:     "not a JSON object",
		``:         "not a JSON object",
		`{"a":1}x`: "not a JSON object",
	} {
		_, err := objectOf(json.RawMessage(raw))
		if err == nil {
			t.Errorf("objectOf(%s) accepted", raw)
			continue
		}
		if !strings.Contains(err.Error(), hint) {
			t.Errorf("objectOf(%s) = %v, want %q", raw, err, hint)
		}
	}
}
