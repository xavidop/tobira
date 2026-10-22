package duckdb

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/plugin/convert"
)

func TestParseType(t *testing.T) {
	cases := map[string]string{
		"INTEGER":                                  "INTEGER",
		"DECIMAL(10,2)":                            "DECIMAL(10,2)",
		"INTEGER[]":                                "list<INTEGER>",
		"INTEGER[2]":                               "list<INTEGER>",
		"VARCHAR[][]":                              "list<list<VARCHAR>>",
		`STRUCT("b" INTEGER, "a" VARCHAR)`:         "struct<b:INTEGER,a:VARCHAR>",
		`STRUCT("x" STRUCT("y" INTEGER)[])`:        "struct<x:list<struct<y:INTEGER>>>",
		`STRUCT("c""d" INTEGER, "a b" DOUBLE)`:     `struct<c"d:INTEGER,a b:DOUBLE>`,
		"MAP(VARCHAR, INTEGER)":                    "map<VARCHAR,INTEGER>",
		"MAP(VARCHAR, STRUCT(\"k\" INTEGER))":      "map<VARCHAR,struct<k:INTEGER>>",
		`UNION("num" INTEGER, "str" VARCHAR)`:      "union<num:INTEGER,str:VARCHAR>",
		`STRUCT("m" MAP(INTEGER, DECIMAL(4,1)[]))`: "struct<m:map<INTEGER,list<DECIMAL(4,1)>>>",
	}
	for in, want := range cases {
		if got := parseType(in).String(); got != want {
			t.Errorf("parseType(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestMapType(t *testing.T) {
	cases := map[string]driver.Type{
		"BOOLEAN": driver.TypeBool, "TINYINT": driver.TypeInt, "INTEGER": driver.TypeInt, "BIGINT": driver.TypeInt,
		"UINTEGER": driver.TypeInt, "UBIGINT": driver.TypeDecimal, "HUGEINT": driver.TypeDecimal,
		"UHUGEINT": driver.TypeDecimal, "BIGNUM": driver.TypeDecimal, "VARINT": driver.TypeDecimal,
		"FLOAT": driver.TypeFloat, "DOUBLE": driver.TypeFloat, "DECIMAL(10,2)": driver.TypeDecimal,
		"VARCHAR": driver.TypeString, "ENUM": driver.TypeString, "BIT": driver.TypeString, "UUID": driver.TypeString,
		"INTERVAL": driver.TypeString, "BLOB": driver.TypeBytes, "JSON": driver.TypeJSON,
		"DATE": driver.TypeTime, "TIME": driver.TypeTime, "TIMETZ": driver.TypeTime, "TIMESTAMP": driver.TypeTime,
		"TIMESTAMPTZ": driver.TypeTime, "TIMESTAMP WITH TIME ZONE": driver.TypeTime, "TIMESTAMP_NS": driver.TypeTime,
		"TIMESTAMP_MS": driver.TypeTime, "TIMESTAMP_S": driver.TypeTime,
		"INTEGER[]": driver.TypeList, "INTEGER[3]": driver.TypeList, `STRUCT("a" INTEGER)`: driver.TypeObject,
		"MAP(VARCHAR, INTEGER)": driver.TypeObject, `UNION("a" INTEGER)`: driver.TypeObject,
		"GEOMETRY": driver.TypeBytes, "SOMETHING_NEW": driver.TypeUnknown,
	}
	for in, want := range cases {
		if got := mapType(in); got != want {
			t.Errorf("mapType(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestValues reads every DuckDB type family through Query and checks the
// driver value it becomes.
func TestValues(t *testing.T) {
	path := newFile(t, "create type mood as enum ('sad', 'ok')")
	c := open(t, path, false)
	const q = `select
		12.50::decimal(10,2) as dec, (-5)::decimal(4,2) as decneg, 7::decimal(18,0) as dec0,
		12345678901234567890.123456789::decimal(38,9) as decbig,
		170141183460469231731687303715884105727::hugeint as huge,
		340282366920938463463374607431768211455::uhugeint as uhuge,
		18446744073709551615::ubigint as ubig, 12345678901234567890123::varint as vint,
		5::utinyint as utiny, (-3)::tinyint as tiny,
		interval 1 year + interval 2 month + interval 3 day + interval 5 second + interval 250 millisecond as ival,
		(-interval 1 day) as ivalneg, interval 1 day + interval 3 hour as ival1,
		[1, 2, null] as lst, [1, 2]::int[2] as arr,
		{'b': 1, 'a': 'x'} as st, {'x': [{'y': 2}]} as nested,
		map {'k1': 1, 'k0': 2} as mp, map {2: 'two', 1: 'one'} as mpint,
		union_value(num := 2)::union(num int, str varchar) as un,
		uuid '6ba7b810-9dad-11d1-80b4-00c04fd430c8' as uid,
		'ab'::blob as bl, '101'::bit as bt, 'ok'::mood as en,
		timestamp '2026-01-02 03:04:05.123456' as ts,
		timestamptz '2026-01-02 03:04:05+02' as tstz,
		timestamp_ns '2026-01-02 03:04:05.123456789' as tsns,
		timestamp_ms '2026-01-02 03:04:05.123' as tsms,
		timestamp_s '2026-01-02 03:04:05' as tss,
		date '2026-01-02' as dt, time '03:04:05' as tm,
		'{"z": 1, "a": [1, 2]}'::json as js,
		1.5::float as fl, 2.25::double as db, true as bo, null::int as nul, 'text' as vc`
	rs, err := c.Query(context.Background(), driver.Query{Text: q})
	if err != nil {
		t.Fatal(err)
	}
	cols := rs.Columns()
	rows, _, err := driver.Collect(context.Background(), rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]driver.Value{}
	types := map[string]driver.Type{}
	for i, col := range cols {
		got[col.Name] = rows[0][i]
		types[col.Name] = col.Type
	}
	text := func(name, want string, typ driver.Type) {
		t.Helper()
		v := got[name]
		if v.Type() != typ || v.String() != want {
			t.Errorf("%s = %s %q, want %s %q", name, v.Type(), v.String(), typ, want)
		}
		if types[name] != typ {
			t.Errorf("column %s Type = %s, want %s", name, types[name], typ)
		}
	}
	text("dec", "12.50", driver.TypeDecimal)
	text("decneg", "-5.00", driver.TypeDecimal)
	text("dec0", "7", driver.TypeDecimal)
	text("decbig", "12345678901234567890.123456789", driver.TypeDecimal)
	text("huge", "170141183460469231731687303715884105727", driver.TypeDecimal)
	text("uhuge", "340282366920938463463374607431768211455", driver.TypeDecimal)
	text("ubig", "18446744073709551615", driver.TypeDecimal)
	text("vint", "12345678901234567890123", driver.TypeDecimal)
	text("utiny", "5", driver.TypeInt)
	text("tiny", "-3", driver.TypeInt)
	text("ival", "1 year 2 months 3 days 00:00:05.25", driver.TypeString)
	text("ivalneg", "-1 day", driver.TypeString)
	text("ival1", "1 day 03:00:00", driver.TypeString)
	text("uid", "6ba7b810-9dad-11d1-80b4-00c04fd430c8", driver.TypeString)
	text("bt", "101", driver.TypeString)
	text("en", "ok", driver.TypeString)
	text("vc", "text", driver.TypeString)
	text("bo", "true", driver.TypeBool)
	if b, ok := got["bl"].AsBytes(); !ok || string(b) != "ab" {
		t.Errorf("bl = %v, want bytes ab", got["bl"])
	}
	if f, ok := got["fl"].AsFloat(); !ok || f != 1.5 {
		t.Errorf("fl = %v", got["fl"])
	}
	if !got["nul"].IsNull() || types["nul"] != driver.TypeInt {
		t.Errorf("nul = %v (%s)", got["nul"], types["nul"])
	}

	if l, ok := got["lst"].AsList(); !ok || len(l) != 3 || l[0].String() != "1" || !l[2].IsNull() {
		t.Errorf("lst = %v", got["lst"])
	}
	if l, ok := got["arr"].AsList(); !ok || len(l) != 2 {
		t.Errorf("arr = %v", got["arr"])
	}
	keys := func(name string) string {
		t.Helper()
		o, ok := got[name].AsObject()
		if !ok {
			t.Fatalf("%s = %v, want an object", name, got[name])
		}
		var ks []string
		for _, m := range o {
			ks = append(ks, m.Key+"="+m.Value.String())
		}
		return strings.Join(ks, ",")
	}
	if k := keys("st"); k != "b=1,a=x" {
		t.Errorf("struct keys = %s, want declaration order b=1,a=x", k)
	}
	if k := keys("mp"); k != "k1=1,k0=2" {
		t.Errorf("map = %s, want k1=1,k0=2 in map order", k)
	}
	if k := keys("mpint"); k != "2=two,1=one" {
		t.Errorf("int map = %s, want 2=two,1=one", k)
	}
	if k := keys("un"); k != "num=2" {
		t.Errorf("union = %s, want num=2", k)
	}
	n, _ := got["nested"].AsObject()
	if inner, ok := n[0].Value.AsList(); !ok || len(inner) != 1 || inner[0].Type() != driver.TypeObject {
		t.Errorf("nested = %v", got["nested"])
	}

	tm := func(name string, want time.Time) {
		t.Helper()
		v, ok := got[name].AsTime()
		if !ok || !v.Equal(want) {
			t.Errorf("%s = %v, want %v", name, got[name], want)
		}
	}
	tm("ts", time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC))
	tm("tstz", time.Date(2026, 1, 2, 1, 4, 5, 0, time.UTC))
	tm("tsns", time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC))
	tm("tsms", time.Date(2026, 1, 2, 3, 4, 5, 123000000, time.UTC))
	tm("tss", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	tm("dt", time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if v, ok := got["tm"].AsTime(); !ok || v.Hour() != 3 || v.Minute() != 4 || v.Second() != 5 {
		t.Errorf("tm = %v", got["tm"])
	}

	raw, ok := got["js"].AsJSON()
	if !ok {
		t.Fatalf("js = %v, want JSON", got["js"])
	}
	var js map[string]any
	if err := json.Unmarshal(raw, &js); err != nil || js["z"] != float64(1) {
		t.Errorf("js = %s (%v)", raw, err)
	}
}

func TestLargeValuesAreCapped(t *testing.T) {
	path := newFile(t)
	c := open(t, path, false)
	rs, err := c.Query(context.Background(), driver.Query{Text: "select repeat('x', 2000000) as big, range(0, 300000) as many"})
	if err != nil {
		t.Fatal(err)
	}
	rows, st, err := driver.Collect(context.Background(), rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := rows[0][0].AsText()
	if len(s) > maxValueBytes+16 || !strings.HasPrefix(s, "xxx") {
		t.Errorf("big string is %d bytes, want it cut near %d", len(s), maxValueBytes)
	}
	if l, ok := rows[0][1].AsList(); !ok || len(l) > maxListItems {
		t.Errorf("list has %d items, want at most %d", len(l), maxListItems)
	}
	if len(st.Warnings) == 0 {
		t.Error("no warning about the cut values")
	}
}

// TestValuesFitTheProtocol round-trips results through the plugin
// protocol's encoder: a value the driver returns must never break its 1 MiB
// cap, whatever the list, map or struct holds; it is cut with a warning.
func TestValuesFitTheProtocol(t *testing.T) {
	c := open(t, newFile(t), false)
	for _, q := range []string{
		"select list_transform(range(10000), x -> repeat('z', 200)) as v",
		"select [repeat('a', 400000), repeat('b', 400000), repeat('c', 400000)] as v",
		"select map_from_entries(list_transform(range(5000), x -> {'k': x::varchar, 'v': repeat('m', 300)})) as v",
		"select {'a': repeat('a', 500000), 'b': repeat('b', 500000), 'c': repeat('c', 500000)} as v",
		"select [[repeat('n', 300000), repeat('n', 300000)], [repeat('n', 300000), repeat('n', 300000)]] as v",
		"select list_transform(range(3000), x -> {'s': repeat('s', 400), 'l': [x, x]}) as v",
	} {
		rs, err := c.Query(context.Background(), driver.Query{Text: q})
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		rows, st, err := driver.Collect(context.Background(), rs, 0)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if _, err := convert.ValueToProto(rows[0][0]); err != nil {
			t.Errorf("%s: the value breaks the protocol: %v", q, err)
		}
		if len(st.Warnings) == 0 {
			t.Errorf("%s: no warning about the cut value", q)
		}
	}
	// A value that fits is not cut.
	rs, err := c.Query(context.Background(), driver.Query{Text: "select list_transform(range(100), x -> repeat('z', 100)) as v"})
	if err != nil {
		t.Fatal(err)
	}
	rows, st, err := driver.Collect(context.Background(), rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	if l, _ := rows[0][0].AsList(); len(l) != 100 || len(st.Warnings) != 0 {
		t.Errorf("a small list came back with %d items and warnings %v", len(l), st.Warnings)
	}
}
