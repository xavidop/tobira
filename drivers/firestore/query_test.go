package firestore

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestParseQuery(t *testing.T) {
	q, err := parseQuery(`{"collection":"orders","where":[["status","==","paid"],["total",">=",10],["a.b","in",[1,2]]],`+
		`"orderBy":["total",["created","desc"]],"limit":5,"offset":2,"select":["status","a.b"]}`, false)
	if err != nil {
		t.Fatal(err)
	}
	if q.collection != "orders" || q.limit != 5 || !q.hasLimit || q.offset != 2 {
		t.Errorf("spec = %+v", q)
	}
	if len(q.where) != 3 || !slices.Equal(q.where[2].path, []string{"a", "b"}) || q.where[2].op != "in" || q.where[1].value != int64(10) {
		t.Errorf("where = %+v", q.where)
	}
	if len(q.orderBy) != 2 || q.orderBy[0].desc || !q.orderBy[1].desc || !slices.Equal(q.orderBy[1].path, []string{"created"}) {
		t.Errorf("orderBy = %+v", q.orderBy)
	}
	if !q.hasSelect || len(q.selectPaths) != 2 || !slices.Equal(q.selectPaths[1], []string{"a", "b"}) {
		t.Errorf("select = %+v", q.selectPaths)
	}

	for _, text := range []string{
		`{"collection":"a/doc/b"}`,
		`{"collectionGroup":"b","where":[["x","array-contains","y"]]}`,
		`{"document":"a/doc"}`,
		`{"document":"a/doc/b/doc2"}`,
		`{"collection":"a","count":true}`,
		`{"collection":"a","where":[["_id","==","x"]]}`,
		`{"collection":"a","where":[["x","not-in",[1]],["y","array-contains-any",[1]],["z","!=",null],["w","<",1],["v","<=",1],["u",">",1]]}`,
		` {"collection":"a"} `,
	} {
		if _, err := parseQuery(text, true); err != nil {
			t.Errorf("parseQuery(%s): %v", text, err)
		}
	}
}

func TestFieldPath(t *testing.T) {
	for in, want := range map[string][]string{
		"a":               {"a"},
		"a.b.c":           {"a", "b", "c"},
		"`a.b`":           {"a.b"},
		"`a.b`.c":         {"a.b", "c"},
		"x.`y.z`":         {"x", "y.z"},
		"`we\\\\ird\\``":  {"we\\ird`"},
		"_id":             {docIDField},
		"__name__":        {docIDField},
		"`_id`":           {"_id"},
		"_id_field":       {"_id"},
		"`with space`.ok": {"with space", "ok"},
	} {
		got, err := fieldPath(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("fieldPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", ".", "a.", ".a", "a..b", "`a", "`a`b", "a`b`", "``", "`a`.", "`a\\"} {
		if _, err := fieldPath(bad); err == nil {
			t.Errorf("fieldPath(%q) accepted", bad)
		}
	}
}

func TestParseQuerySyntax(t *testing.T) {
	for _, text := range []string{
		`{"collection":""}`,
		`{"collection":"a/b"}`,
		`{"collection":"/a"}`,
		`{"collection":"a//b/c"}`,
		`{"collection":5}`,
		`{"collectionGroup":"a/b/c"}`,
		`{"document":"a"}`,
		`{"document":"a/b","where":[]}`,
		`{"document":"a/b","limit":1}`,
		`{"collection":"a","document":"a/b"}`,
		`{"collection":"a","collectionGroup":"a"}`,
		`{"where":[]}`,
		`{"collection":"a","where":[["x","=","1"]]}`,
		`{"collection":"a","where":[["x","=="]]}`,
		`{"collection":"a","where":[["","==",1]]}`,
		`{"collection":"a","where":[["a..b","==",1]]}`,
		`{"collection":"a","where":"x"}`,
		`{"collection":"a","where":[["x","in","notalist"]]}`,
		`{"collection":"a","where":[["x","==",{"$nope":1}]]}`,
		`{"collection":"a","orderBy":[["x","sideways"]]}`,
		`{"collection":"a","orderBy":[5]}`,
		`{"collection":"a","limit":-1}`,
		`{"collection":"a","limit":1.5}`,
		`{"collection":"a","offset":-1}`,
		`{"collection":"a","select":"x"}`,
		`{"collection":"a","count":"yes"}`,
		`{"collection":"a","count":true,"select":["x"]}`,
	} {
		_, err := parseQuery(text, false)
		if !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("parseQuery(%s): want ErrSyntax, got %v", text, err)
		}
		// A bad value inside a known key is still a syntax error on a
		// read-only call: the request's shape is known to be a read.
		if _, err := parseQuery(text, true); !errors.Is(err, driver.ErrSyntax) && !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("parseQuery(%s, ro): want ErrSyntax or ErrReadOnly, got %v", text, err)
		}
	}
}

// The read-only guard: a request is an allow-listed set of read keys.
// Anything else is refused, as ErrReadOnly on a read-only call.
func TestParseQueryGuard(t *testing.T) {
	writes := []string{
		`{"delete":"tobira_people/1"}`,
		`{"set":"a/b","data":{"x":1}}`,
		`{"update":"a/b","data":{"x":1}}`,
		`{"create":"a/b","data":{}}`,
		`{"add":"a","data":{}}`,
		`{"batch":[{"delete":"a/b"}]}`,
		`{"commit":{"writes":[]}}`,
		`{"transaction":[]}`,
		`{"collection":"a","delete":true}`,
		`{"collection":"a","data":{"x":1}}`,
		`{"collection":"a","Collection":"b"}`,
		`{"collection":"a","collection":"b"}`,
		`{"collection":"a"} {"delete":"a/b"}`,
		`{"collection":"a"}x`,
		`[{"collection":"a"}]`,
		`"collection"`,
		``,
		`{`,
		`{"collection":"a",}`,
	}
	for _, text := range writes {
		_, err := parseQuery(text, true)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("parseQuery(%s, ro): want ErrReadOnly, got %v", text, err)
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name {
			t.Errorf("parseQuery(%s, ro): not a driver error: %T", text, err)
		}
		_, err = parseQuery(text, false)
		if err == nil || errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("parseQuery(%s): want a refusal that is not ErrReadOnly, got %v", text, err)
		}
	}
	// A write verb is ErrUnsupported on a writable call, with the hint.
	_, err := parseQuery(`{"delete":"a/b"}`, false)
	if !errors.Is(err, driver.ErrUnsupported) || !strings.Contains(err.Error(), "editor") {
		t.Errorf("write verb: %v", err)
	}
}
