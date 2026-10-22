package influxdb

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

// cpuTable is the cpu measurement as Describe reports it.
func cpuTable() *table {
	return &table{
		ref: driver.CollectionRef{Namespace: "db", Name: "cpu"},
		fields: []driver.Field{
			{Name: "host", Type: driver.TypeString, NativeType: "Dictionary(Int32, Utf8)", Comment: "tag"},
			{Name: "region", Type: driver.TypeString, NativeType: "Dictionary(Int32, Utf8)", Comment: "tag"},
			{Name: "time", Type: driver.TypeTime, NativeType: "Timestamp(ns)", Comment: "timestamp"},
			{Name: "usage_user", Type: driver.TypeFloat, NativeType: "Float64", Comment: "field"},
			{Name: "Usage", Type: driver.TypeFloat, NativeType: "Float64", Comment: "field"},
			{Name: "usage", Type: driver.TypeFloat, NativeType: "Float64", Comment: "field"},
			{Name: `we"ird`, Type: driver.TypeInt, NativeType: "Int64", Comment: "field"},
		},
	}
}

func TestBuildBrowse(t *testing.T) {
	tb := cpuTable()
	got, err := buildBrowse(tb, driver.BrowseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT * FROM "cpu" ORDER BY "time" DESC, "host", "region" LIMIT 101 OFFSET 0`
	if got.sql != want || got.limit != 100 || got.offset != 0 || len(got.params) != 0 {
		t.Errorf("default browse = %+v, want %q limit 100", got, want)
	}

	got, err = buildBrowse(tb, driver.BrowseOptions{
		Limit:  20,
		Cursor: "o:40",
		Sort:   []driver.SortKey{{Field: "USAGE_USER", Desc: true}, {Field: "time"}},
		Filter: driver.Filter{
			Conditions: []driver.Condition{
				{Field: "host", Op: driver.OpEq, Value: driver.String("web-1")},
				{Field: "usage_user", Op: driver.OpGe, Value: driver.Float(1.5)},
				{Field: "region", Op: driver.OpIn, Value: driver.List(driver.String("eu"), driver.String("us"))},
				{Field: `we"ird`, Op: driver.OpContains, Value: driver.String("7")},
				{Field: "host", Op: driver.OpPrefix, Value: driver.String("web")},
				{Field: "region", Op: driver.OpIsNull},
				{Field: "host", Op: driver.OpNe, Value: driver.Null()},
				{Field: "time", Op: driver.OpLt, Value: driver.Time(time.Unix(0, 1767225600000000000))},
				{Field: "region", Op: driver.OpIn, Value: driver.List()},
			},
			Raw: "usage_user < 99",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want = `SELECT * FROM "cpu" WHERE "host" = $1 AND "usage_user" >= $2 AND "region" IN ($3, $4)` +
		` AND strpos(CAST("we""ird" AS VARCHAR), $5) > 0 AND starts_with(CAST("host" AS VARCHAR), $6)` +
		` AND "region" IS NULL AND "host" IS NOT NULL AND "time" < to_timestamp_nanos(1767225600000000000) AND FALSE` +
		" AND (usage_user < 99\n)" +
		` ORDER BY "usage_user" DESC, "time", "host", "region" LIMIT 21 OFFSET 40`
	if got.sql != want {
		t.Errorf("browse sql =\n%s\nwant\n%s", got.sql, want)
	}
	wantParams := map[string]any{"1": "web-1", "2": 1.5, "3": "eu", "4": "us", "5": "7", "6": "web"}
	if !reflect.DeepEqual(got.params, wantParams) {
		t.Errorf("params = %v, want %v", got.params, wantParams)
	}
	if got.limit != 20 || got.offset != 40 {
		t.Errorf("limit/offset = %d/%d", got.limit, got.offset)
	}

	got, err = buildBrowse(tb, driver.BrowseOptions{Limit: 1_000_000})
	if err != nil || got.limit != maxPage {
		t.Errorf("a huge Limit = %d (%v), want the cap %d", got.limit, err, maxPage)
	}
}

func TestBuildBrowseErrors(t *testing.T) {
	tb := cpuTable()
	for _, tc := range []struct {
		name string
		opts driver.BrowseOptions
		kind error
	}{
		{"unknown sort", driver.BrowseOptions{Sort: []driver.SortKey{{Field: "nope"}}}, driver.ErrNotFound},
		{"ambiguous field", driver.BrowseOptions{Sort: []driver.SortKey{{Field: "USAGE"}}}, driver.ErrNotFound},
		{"unknown condition", driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "x", Op: driver.OpEq, Value: driver.Int(1)}}}}, driver.ErrNotFound},
		{"bad op", driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "host", Op: "regex", Value: driver.String("x")}}}}, driver.ErrUnsupported},
		{"in without list", driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "host", Op: driver.OpIn, Value: driver.String("x")}}}}, driver.ErrSyntax},
		{"lt null", driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "host", Op: driver.OpLt, Value: driver.Null()}}}}, driver.ErrSyntax},
		{"nan", driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "usage_user", Op: driver.OpEq, Value: driver.Float(math.NaN())}}}}, driver.ErrUnsupported},
		{"bytes", driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "host", Op: driver.OpEq, Value: driver.Bytes([]byte("x"))}}}}, driver.ErrUnsupported},
		{"bad cursor", driver.BrowseOptions{Cursor: "k:abc"}, driver.ErrSyntax},
		{"time out of range", driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{
			{Field: "time", Op: driver.OpLt, Value: driver.Time(time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC))}}}}, driver.ErrSyntax},
		{"time out of range in a list", driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{
			{Field: "time", Op: driver.OpIn, Value: driver.List(driver.Time(time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC)))}}}}, driver.ErrSyntax},
		{"negative cursor", driver.BrowseOptions{Cursor: "o:-5"}, driver.ErrSyntax},
		{"raw escape", driver.BrowseOptions{Filter: driver.Filter{Raw: "1=1) OR (1=1"}}, driver.ErrSyntax},
	} {
		_, err := buildBrowse(tb, tc.opts)
		if !errors.Is(err, tc.kind) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.kind)
		}
	}
}

func TestBuildRange(t *testing.T) {
	tb := cpuTable()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)
	got, err := buildRange(tb, driver.RangeQuery{Field: "usage_user", Start: start, End: end, Window: 30 * time.Minute, Aggregate: "mean"})
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT date_bin(INTERVAL '1800000000000 nanoseconds', "time") AS "_time", avg("usage_user") AS "value"` +
		` FROM "cpu" WHERE "time" >= to_timestamp_nanos(1767225600000000000) AND "time" < to_timestamp_nanos(1767232800000000000)` +
		` AND "usage_user" IS NOT NULL GROUP BY "_time" ORDER BY "_time"`
	if got.sql != want {
		t.Errorf("range sql =\n%s\nwant\n%s", got.sql, want)
	}

	got, err = buildRange(tb, driver.RangeQuery{
		Field: "usage_user", Window: time.Minute, Aggregate: "last", GroupBy: []string{"HOST"},
		Filter: driver.Filter{Conditions: []driver.Condition{{Field: "region", Op: driver.OpEq, Value: driver.String("eu")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want = `SELECT date_bin(INTERVAL '60000000000 nanoseconds', "time") AS "_time", "host", last_value("usage_user" ORDER BY "time") AS "value"` +
		` FROM "cpu" WHERE "region" = $1 AND "usage_user" IS NOT NULL GROUP BY "_time", "host" ORDER BY "_time", "host"`
	if got.sql != want || !reflect.DeepEqual(got.params, map[string]any{"1": "eu"}) {
		t.Errorf("grouped range =\n%s %v\nwant\n%s", got.sql, got.params, want)
	}

	got, err = buildRange(tb, driver.RangeQuery{Field: "usage_user", Start: start, Aggregate: "max"})
	if err != nil {
		t.Fatal(err)
	}
	want = `SELECT "time" AS "_time", "usage_user" AS "value" FROM "cpu"` +
		` WHERE "time" >= to_timestamp_nanos(1767225600000000000) AND "usage_user" IS NOT NULL ORDER BY "_time"`
	if got.sql != want {
		t.Errorf("raw range =\n%s\nwant\n%s", got.sql, want)
	}

	for agg, fn := range map[string]string{"": "avg", "sum": "sum", "min": "min", "count": "count", "MEAN": "avg"} {
		got, err := buildRange(tb, driver.RangeQuery{Field: "usage_user", Window: time.Second, Aggregate: agg})
		if err != nil {
			t.Errorf("aggregate %q: %v", agg, err)
			continue
		}
		if !strings.Contains(got.sql, fn+`("usage_user")`) {
			t.Errorf("aggregate %q: sql %s does not use %s", agg, got.sql, fn)
		}
	}
}

func TestBuildRangeErrors(t *testing.T) {
	tb := cpuTable()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		q    driver.RangeQuery
		kind error
	}{
		{"no field", driver.RangeQuery{}, driver.ErrSyntax},
		{"unknown field", driver.RangeQuery{Field: "nope"}, driver.ErrNotFound},
		{"time field", driver.RangeQuery{Field: "time"}, driver.ErrSyntax},
		{"unknown group", driver.RangeQuery{Field: "usage_user", GroupBy: []string{"nope"}}, driver.ErrNotFound},
		{"bad aggregate", driver.RangeQuery{Field: "usage_user", Window: time.Second, Aggregate: "median"}, driver.ErrUnsupported},
		{"negative window", driver.RangeQuery{Field: "usage_user", Window: -time.Second}, driver.ErrSyntax},
		{"end before start", driver.RangeQuery{Field: "usage_user", Start: start, End: start.Add(-time.Hour)}, driver.ErrSyntax},
		{"start before 1678", driver.RangeQuery{Field: "usage_user", Start: time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)}, driver.ErrSyntax},
		{"end after 2262", driver.RangeQuery{Field: "usage_user", End: time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)}, driver.ErrSyntax},
		{"filter time out of range", driver.RangeQuery{Field: "usage_user", Filter: driver.Filter{Conditions: []driver.Condition{
			{Field: "time", Op: driver.OpGt, Value: driver.Time(time.Time{}.AddDate(1, 0, 0))}}}}, driver.ErrSyntax},
	} {
		_, err := buildRange(tb, tc.q)
		if !errors.Is(err, tc.kind) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.kind)
		}
	}
}

func TestQueryParams(t *testing.T) {
	got, err := queryParams([]driver.Value{driver.Int(7), driver.String("x"), driver.Bool(true), driver.Float(2.5), driver.Decimal("1.10"), driver.Null(),
		driver.Time(time.Date(2026, 1, 1, 0, 0, 0, 5, time.UTC))})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"1": int64(7), "2": "x", "3": true, "4": 2.5, "5": "1.10", "6": nil, "7": "2026-01-01T00:00:00.000000005Z"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("queryParams = %v, want %v", got, want)
	}
	if got, err := queryParams(nil); got != nil || err != nil {
		t.Errorf("queryParams(nil) = %v, %v", got, err)
	}
	for _, v := range []driver.Value{driver.Bytes([]byte("x")), driver.List(), driver.Float(math.NaN())} {
		if _, err := queryParams([]driver.Value{v}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("queryParams(%s) = %v, want ErrUnsupported", v, err)
		}
	}
}
