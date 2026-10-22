package firestore

import (
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestCursorRoundTrip(t *testing.T) {
	opts := driver.BrowseOptions{Sort: []driver.SortKey{{Field: "age", Desc: true}},
		Filter: driver.Filter{Conditions: []driver.Condition{{Field: "a", Op: driver.OpEq, Value: driver.Int(1)}}, Raw: `[["b","==",2]]`}}
	sig := pageSignature("users", opts)
	c := encodeCursor(sig, "doc-1")
	id, err := decodeCursor(c, sig)
	if err != nil || id != "doc-1" {
		t.Fatalf("decode = %q, %v", id, err)
	}
	if id, err := decodeCursor("", sig); err != nil || id != "" {
		t.Errorf("empty cursor = %q, %v", id, err)
	}
	// A cursor is for one collection, sort and filter.
	other := func(mod func(*driver.BrowseOptions)) driver.BrowseOptions {
		o := opts
		o.Filter.Conditions = append([]driver.Condition(nil), opts.Filter.Conditions...)
		mod(&o)
		return o
	}
	for name, sig2 := range map[string]string{
		"sort":       pageSignature("users", other(func(o *driver.BrowseOptions) { o.Sort = nil })),
		"collection": pageSignature("users/ada/orders", opts),
		"condition":  pageSignature("users", other(func(o *driver.BrowseOptions) { o.Filter.Conditions[0].Value = driver.Int(2) })),
		"value type": pageSignature("users", other(func(o *driver.BrowseOptions) { o.Filter.Conditions[0].Value = driver.String("1") })),
		"raw":        pageSignature("users", other(func(o *driver.BrowseOptions) { o.Filter.Raw = `[["b","==",3]]` })),
	} {
		if _, err := decodeCursor(c, sig2); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("cursor reused with another %s: %v", name, err)
		}
	}
	if pageSignature("users", opts) != sig {
		t.Error("pageSignature is not deterministic")
	}
	for _, bad := range []string{"x", "d:!!", "d:" + strings.Repeat("A", 4096), encodeCursor(sig, ""), encodeCursor(sig, "a/b")} {
		if _, err := decodeCursor(bad, sig); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("decodeCursor(%q): want ErrSyntax, got %v", bad, err)
		}
	}
}

func TestConditionClauses(t *testing.T) {
	tests := []struct {
		c    driver.Condition
		want []whereClause
	}{
		{driver.Condition{Field: "a", Op: driver.OpEq, Value: driver.Int(1)}, []whereClause{{path: []string{"a"}, op: "==", value: int64(1)}}},
		{driver.Condition{Field: "a.b", Op: driver.OpNe, Value: driver.String("x")}, []whereClause{{path: []string{"a.b"}, op: "!=", value: "x"}}},
		{driver.Condition{Field: "a", Op: driver.OpLt, Value: driver.Float(1)}, []whereClause{{path: []string{"a"}, op: "<", value: 1.0}}},
		{driver.Condition{Field: "a", Op: driver.OpLe, Value: driver.Int(1)}, []whereClause{{path: []string{"a"}, op: "<=", value: int64(1)}}},
		{driver.Condition{Field: "a", Op: driver.OpGt, Value: driver.Int(1)}, []whereClause{{path: []string{"a"}, op: ">", value: int64(1)}}},
		{driver.Condition{Field: "a", Op: driver.OpGe, Value: driver.Int(1)}, []whereClause{{path: []string{"a"}, op: ">=", value: int64(1)}}},
		{driver.Condition{Field: "a", Op: driver.OpIsNull}, []whereClause{{path: []string{"a"}, op: "==", value: nil}}},
		{driver.Condition{Field: "a", Op: driver.OpNotNull}, []whereClause{{path: []string{"a"}, op: "!=", value: nil}}},
		{driver.Condition{Field: "a", Op: driver.OpIn, Value: driver.List(driver.Int(1), driver.Int(2))}, []whereClause{{path: []string{"a"}, op: "in", value: []any{int64(1), int64(2)}}}},
		{driver.Condition{Field: "a", Op: driver.OpPrefix, Value: driver.String("ab")}, []whereClause{
			{path: []string{"a"}, op: ">=", value: "ab"}, {path: []string{"a"}, op: "<", value: "ab"}}},
		{driver.Condition{Field: "_id", Op: driver.OpEq, Value: driver.String("d1")}, []whereClause{{path: []string{docIDField}, op: "==", value: "d1"}}},
		// A stored field named _id shows as _id_field.
		{driver.Condition{Field: "_id_field", Op: driver.OpEq, Value: driver.Int(1)}, []whereClause{{path: []string{"_id"}, op: "==", value: int64(1)}}},
		// Backquotes make a path: a nested field, or one whose name has dots.
		{driver.Condition{Field: "`address`.city", Op: driver.OpEq, Value: driver.String("L")}, []whereClause{{path: []string{"address", "city"}, op: "==", value: "L"}}},
		{driver.Condition{Field: "`a.b`", Op: driver.OpEq, Value: driver.Int(1)}, []whereClause{{path: []string{"a.b"}, op: "==", value: int64(1)}}},
	}
	for _, tt := range tests {
		got, err := conditionClauses(tt.c)
		if err != nil {
			t.Errorf("%+v: %v", tt.c, err)
			continue
		}
		if len(got) != len(tt.want) {
			t.Errorf("%+v: got %+v", tt.c, got)
			continue
		}
		for i := range got {
			if strings.Join(got[i].path, ".") != strings.Join(tt.want[i].path, ".") || len(got[i].path) != len(tt.want[i].path) ||
				got[i].op != tt.want[i].op || !equalAny(got[i].value, tt.want[i].value) {
				t.Errorf("%+v: clause %d = %+v, want %+v", tt.c, i, got[i], tt.want[i])
			}
		}
	}
	for _, c := range []driver.Condition{
		{Field: "a", Op: driver.OpContains, Value: driver.String("x")},
		{Field: "a", Op: driver.OpPrefix, Value: driver.Int(1)},
		{Field: "a", Op: "regex", Value: driver.String("x")},
	} {
		if _, err := conditionClauses(c); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%+v: want ErrUnsupported, got %v", c, err)
		}
	}
	for _, c := range []driver.Condition{
		{Field: "", Op: driver.OpEq, Value: driver.Int(1)},
		{Field: "a", Op: driver.OpIn, Value: driver.Int(1)},
		{Field: "_id", Op: driver.OpEq, Value: driver.Int(1)},
		{Field: "`a", Op: driver.OpEq, Value: driver.Int(1)},
	} {
		if _, err := conditionClauses(c); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("%+v: want ErrSyntax, got %v", c, err)
		}
	}
}

func TestParseRawFilter(t *testing.T) {
	got, err := parseRawFilter(`[["status","==","paid"],["n",">",1]]`)
	if err != nil || len(got) != 2 || got[1].op != ">" {
		t.Fatalf("raw = %+v, %v", got, err)
	}
	if got, err := parseRawFilter(`  `); err != nil || got != nil {
		t.Errorf("blank raw = %+v, %v", got, err)
	}
	for _, bad := range []string{`{"delete":"a/b"}`, `[["a","==",1]] x`, `[["a","~",1]]`, `nope`} {
		if _, err := parseRawFilter(bad); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("parseRawFilter(%s): want ErrSyntax, got %v", bad, err)
		}
	}
}
