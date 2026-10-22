package weaviate

import (
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestParseQueryRoutes(t *testing.T) {
	for _, tt := range []struct {
		text    string
		graphql bool
	}{
		{`{"collection":"A"}`, false},
		{` { "collection" : "A", "limit": 3 }`, false},
		{`{ Get { A { b } } }`, true},
		{"query { Get { A { b } } }", true},
		{"\n# a comment\n{ Get { A { b } } }", true},
	} {
		p, err := parseQuery(tt.text, false)
		if err != nil {
			t.Fatalf("parseQuery(%s): %v", tt.text, err)
		}
		if (p.gql != nil) != tt.graphql || (p.req != nil) == tt.graphql {
			t.Errorf("parseQuery(%s): graphql=%v req=%v", tt.text, p.gql != nil, p.req != nil)
		}
	}
}

func TestParseRequestFields(t *testing.T) {
	p, err := parseQuery(`{"collection":"Article","fields":["title","n"],"where":{"path":["n"],"operator":"GreaterThan","valueInt":2},
		"sort":[{"path":["n"],"order":"desc"}],"limit":5,"offset":2,"vectors":true}`, true)
	if err != nil {
		t.Fatal(err)
	}
	r := p.req
	if r.collection != "Article" || strings.Join(r.fields, ",") != "title,n" || r.limit == nil || *r.limit != 5 || r.offset != 2 || !r.vectors {
		t.Errorf("request = %+v", r)
	}
	if r.where != `{path: ["n"], operator: GreaterThan, valueInt: 2}` {
		t.Errorf("where = %s", r.where)
	}
	if r.sort != `[{path: ["n"], order: desc}]` {
		t.Errorf("sort = %s", r.sort)
	}
	p, err = parseQuery(`{"collection":"Article","nearVector":{"vector":[1,0.5],"targetVectors":["v"],"distance":0.3}}`, true)
	if err != nil {
		t.Fatal(err)
	}
	if p.req.search != "nearVector" || p.req.searchArg != `{vector: [1, 0.5], targetVectors: ["v"], distance: 0.3}` {
		t.Errorf("search = %s %s", p.req.search, p.req.searchArg)
	}
	p, err = parseQuery(`{"collection":"Article","hybrid":{"query":"x","fusionType":"rankedFusion","alpha":0.5}}`, false)
	if err != nil {
		t.Fatal(err)
	}
	if p.req.searchArg != `{query: "x", fusionType: rankedFusion, alpha: 0.5}` {
		t.Errorf("hybrid = %s", p.req.searchArg)
	}
}

func TestParseRequestRefusals(t *testing.T) {
	for _, tt := range []struct {
		text string
		rw   error // the kind on a writable connection; read-only is always ErrReadOnly
	}{
		{`{}`, driver.ErrSyntax},
		{`[{"collection":"A"}]`, driver.ErrSyntax},
		{`{"collection":"A","op":"delete"}`, driver.ErrSyntax},
		{`{"collection":"A","collection":"B"}`, driver.ErrSyntax},
		{`{"collection":"A"} {"collection":"B"}`, driver.ErrSyntax},
		{`{"collection":"A","delete":true}`, driver.ErrSyntax},
		{`{"collection":"A","objects":[{"id":"x"}]}`, driver.ErrSyntax},
		{`{"collection":1}`, driver.ErrSyntax},
		{`{"collection":"A\"){}"}`, driver.ErrSyntax},
		{`{"collection":"A","fields":["a b"]}`, driver.ErrSyntax},
		{`{"collection":"A","fields":"a"}`, driver.ErrSyntax},
		{`{"collection":"A","nearVector":{"vector":[1]},"bm25":{"query":"x"}}`, driver.ErrSyntax},
		{`{"collection":"A","where":{"path":["a"],"operator":"Equal) { x } }","valueInt":1}}`, driver.ErrSyntax},
		{`{"collection":"A","where":{"path":["a\""],"operator":"Equal","valueInt":1}}`, driver.ErrSyntax},
		{`{"collection":"A","where":{"pa th":["a"]}}`, driver.ErrSyntax},
		{`{"collection":"A","where":"path"}`, driver.ErrSyntax},
		{`{"collection":"A","limit":-1}`, driver.ErrSyntax},
		{`{"collection":"A","limit":1.5}`, driver.ErrSyntax},
		{`{"collection":"A","after":"not-a-uuid"}`, driver.ErrSyntax},
		{`{"collection":"A","after":"00000000-0000-0000-0000-000000000001","where":{"path":["a"],"operator":"IsNull","valueBoolean":true}}`, driver.ErrSyntax},
		{`{"collection":"A","vectors":"yes"}`, driver.ErrSyntax},
		{`mutation { Delete { A } }`, driver.ErrUnsupported},
		{`subscription { x }`, driver.ErrUnsupported},
		{`{ Get { A { b } } } mutation { x }`, driver.ErrSyntax},
		{`DELETE FROM A`, driver.ErrSyntax},
		{``, driver.ErrSyntax},
	} {
		_, err := parseQuery(tt.text, false)
		if !errors.Is(err, tt.rw) {
			t.Errorf("parseQuery(%s, rw): want %v, got %v", tt.text, tt.rw, err)
		}
		_, err = parseQuery(tt.text, true)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("parseQuery(%s, ro): want ErrReadOnly, got %v", tt.text, err)
		}
	}
}

func TestRenderLiteral(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{`"a\"b\\c\n"`, `"a\"b\\c\n"`},
		{`"<script>"`, `"\u003cscript\u003e"`},
		{`12`, `12`},
		{`-1.5e-3`, `-1.5e-3`},
		{`true`, `true`},
		{`null`, `null`},
		{`[1, "x", [false]]`, `[1, "x", [false]]`},
		{`{"geoCoordinates":{"latitude":1,"longitude":2},"distance":{"max":3}}`, `{geoCoordinates: {latitude: 1, longitude: 2}, distance: {max: 3}}`},
		{`{"operator":"And","operands":[{"path":["a"],"operator":"Like","valueText":"x*"}]}`, `{operator: And, operands: [{path: ["a"], operator: Like, valueText: "x*"}]}`},
	} {
		got, err := renderLiteral([]byte(tt.in))
		if err != nil {
			t.Fatalf("renderLiteral(%s): %v", tt.in, err)
		}
		if got != tt.want {
			t.Errorf("renderLiteral(%s) = %s, want %s", tt.in, got, tt.want)
		}
	}
	for _, bad := range []string{`{"a b":1}`, `{"operator":"Equal }"}`, `{"order":"desc, x"}`, `{"a":1,"a":2}`, `{"fusionType":1}`, `[1,`, `{"":1}`} {
		if got, err := renderLiteral([]byte(bad)); err == nil {
			t.Errorf("renderLiteral(%s) = %s, want an error", bad, got)
		}
	}
}
