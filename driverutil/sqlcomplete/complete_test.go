package sqlcomplete

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

// fakeCatalog is an in-memory Catalog. Calls are counted per kind.
type fakeCatalog struct {
	ns    []string
	colls map[string][]string            // ns -> collections
	flds  map[string]map[string][]string // ns -> coll -> fields
	err   error
	mu    sync.Mutex
	calls map[string]int
}

func (f *fakeCatalog) count(k string) {
	f.mu.Lock()
	f.calls[k]++
	f.mu.Unlock()
}

func newFake() *fakeCatalog {
	return &fakeCatalog{
		ns: []string{"main", "other"},
		colls: map[string][]string{
			"main":  {"people", "orders", "Order Items"},
			"other": {"logs"},
		},
		flds: map[string]map[string][]string{
			"main": {
				"people":      {"id", "name", "email"},
				"orders":      {"id", "person_id", "total"},
				"Order Items": {"sku", "Qty", "order"},
			},
			"other": {"logs": {"msg"}},
		},
		calls: map[string]int{},
	}
}

func (f *fakeCatalog) Namespaces(context.Context) ([]string, error) {
	f.count("ns")
	return f.ns, f.err
}

func (f *fakeCatalog) Collections(_ context.Context, ns string) ([]string, error) {
	f.count("coll")
	if ns == "" {
		ns = "main"
	}
	return f.colls[ns], f.err
}

func (f *fakeCatalog) Fields(_ context.Context, ns, coll string) ([]string, error) {
	f.count("fields")
	if f.err != nil {
		return nil, f.err
	}
	if ns == "" {
		ns = "main"
	}
	fs, ok := f.flds[ns][coll]
	if !ok {
		return nil, driver.Wrap(driver.ErrNotFound, "fake", "fields", errors.New(coll+" does not exist"))
	}
	return fs, nil
}

// at splits text at the "|" cursor marker.
func at(text string) driver.CompletionRequest {
	i := strings.IndexByte(text, '|')
	return driver.CompletionRequest{Text: text[:i] + text[i+1:], Cursor: i}
}

func labels(cs []driver.Completion) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Label
	}
	return out
}

func kinds(cs []driver.Completion) map[string]bool {
	m := map[string]bool{}
	for _, c := range cs {
		m[c.Kind] = true
	}
	return m
}

func TestComplete(t *testing.T) {
	sqlite, pg := SQLite(), Postgres()
	tests := []struct {
		name    string
		d       Dialect
		text    string
		want    []string // exact labels, in order; nil means "use contains/absent"
		wantNK  []string // exact labels of the non-keyword completions
		wantNil bool     // expect no completions at all
		has     []string
		hasNot  []string
		onlyKnd string // when set, every completion must be of this kind
	}{
		{name: "after from lists collections and namespaces", d: sqlite, text: "select * from |",
			has: []string{"people", "orders", "Order Items", "main", "other"}, hasNot: []string{"SELECT", "id"}},
		{name: "prefix filters collections", d: sqlite, text: "select * from pe|", want: []string{"people"}},
		{name: "prefix is case-insensitive", d: sqlite, text: "SELECT * FROM PE|", want: []string{"people"}},
		{name: "after join", d: sqlite, text: "select * from people join or|", want: []string{"Order Items", "orders"}},
		{name: "after into", d: sqlite, text: "insert into pe|", want: []string{"people"}},
		{name: "after update", d: sqlite, text: "update pe|", want: []string{"people"}},
		{name: "alias dot lists fields", d: sqlite, text: "select * from people p where p.|", want: []string{"email", "id", "name"}},
		{name: "alias dot with prefix before from", d: sqlite, text: "select p.na| from people p", wantNK: []string{"name"}},
		{name: "AS alias", d: sqlite, text: "select * from people as p where p.|", want: []string{"email", "id", "name"}},
		{name: "second table alias", d: sqlite, text: "select * from people p join orders o on o.|", want: []string{"id", "person_id", "total"}},
		{name: "collection dot", d: sqlite, text: "select * from people where people.|", want: []string{"email", "id", "name"}},
		{name: "collection dot is case-insensitive", d: sqlite, text: "select * from people where PEOPLE.n|", wantNK: []string{"name"}},
		{name: "namespace dot lists its collections", d: sqlite, text: "select * from other.|", want: []string{"logs"}, onlyKnd: "collection"},
		{name: "namespace dot with prefix", d: sqlite, text: "select * from main.pe|", want: []string{"people"}},
		{name: "ns.coll dot lists fields", d: sqlite, text: "select other.logs.| from other.logs", want: []string{"msg"}},
		{name: "select offers fields and keywords", d: sqlite, text: "select | from people",
			has: []string{"id", "name", "email", "DISTINCT"}, hasNot: []string{"people"}},
		{name: "where offers fields", d: sqlite, text: "select * from people where na|", wantNK: []string{"name"}},
		{name: "and offers fields", d: sqlite, text: "select * from people where id = 1 and em|", wantNK: []string{"email"}},
		{name: "order by offers fields", d: sqlite, text: "select * from people order by |", has: []string{"id", "name"}},
		{name: "set offers fields", d: sqlite, text: "update people set |", has: []string{"id", "name"}},
		{name: "fields of every statement collection", d: sqlite, text: "select * from people p, orders o where |",
			has: []string{"id", "name", "person_id", "total"}},
		{name: "insert columns", d: sqlite, text: "insert into people (|", want: nil, has: []string{"id", "name", "email"}},
		{name: "comma in from list offers collections", d: sqlite, text: "select * from people p, |",
			has: []string{"people", "orders"}, hasNot: []string{"id"}},
		{name: "comma in select list offers fields", d: sqlite, text: "select id, | from people", has: []string{"name"}, hasNot: []string{"orders"}},
		{name: "keywords after a table name", d: sqlite, text: "select * from people |",
			has: []string{"WHERE", "JOIN", "ORDER"}, hasNot: []string{"id", "people"}, onlyKnd: "keyword"},
		{name: "keywords at statement start", d: sqlite, text: "sel|", want: []string{"SELECT"}},
		{name: "only the current statement counts", d: sqlite, text: "select * from people; select * from orders where |",
			has: []string{"person_id"}, hasNot: []string{"name", "email"}},
		{name: "statement after the cursor is ignored", d: sqlite, text: "select | from orders; select * from people",
			has: []string{"person_id"}, hasNot: []string{"email"}},
		{name: "cursor mid statement", d: sqlite, text: "select * from pe| where x", want: []string{"people"}},
		{name: "escaped quote in string", d: sqlite, text: "select * from people where name = 'it''s' and em|", wantNK: []string{"email"}},
		{name: "keyword inside string is ignored", d: sqlite, text: "select * from people where name = 'from ' and na|", wantNK: []string{"name"}},
		{name: "inside string", d: sqlite, text: "select * from people where name = 'a from |'", wantNil: true},
		{name: "inside unterminated string", d: sqlite, text: "select * from people where name = 'abc|", wantNil: true},
		{name: "inside line comment", d: sqlite, text: "select 1 -- from |", wantNil: true},
		{name: "after line comment newline", d: sqlite, text: "select 1 -- x\nfrom pe|", want: []string{"people"}},
		{name: "inside block comment", d: sqlite, text: "select /* from | */ 1", wantNil: true},
		{name: "after block comment", d: sqlite, text: "select * from /* c */ pe|", want: []string{"people"}},
		{name: "semicolon in comment does not split", d: sqlite, text: "select * from people p -- ;\n where p.i|", wantNK: []string{"id"}},
		{name: "semicolon in string does not split", d: sqlite, text: "select ';' from people where na|", wantNK: []string{"name"}},
		{name: "quoted identifier prefix keeps quoting", d: sqlite, text: `select * from "Ord|`, want: []string{"Order Items", "orders"}},
		{name: "quoted collection alias", d: sqlite, text: `select * from "Order Items" i where i.|`, want: []string{"order", "Qty", "sku"}},
		{name: "bracket quoting", d: sqlite, text: "select * from [Order Items] i where i.s|", wantNK: []string{"sku"}},
		{name: "backtick quoting", d: sqlite, text: "select * from `Order Items` i where i.s|", wantNK: []string{"sku"}},
		{name: "sqlite parameter", d: sqlite, text: "select * from people where id = :na|", wantNil: true},
		{name: "postgres cast", d: pg, text: "select id::|", wantNil: true},
		{name: "dollar quote hides keyword", d: pg, text: "select $$ from $$ + na| from people", wantNK: []string{"name"}},
		{name: "inside dollar quote", d: pg, text: "select $tag$ from |$tag$", wantNil: true},
		{name: "tagged dollar quote with inner dollars", d: pg, text: "select $a$ $$ ; $a$ + na| from people", wantNK: []string{"name"}},
		{name: "E string with escaped quote", d: pg, text: `select E'it\'s ; ' + na| from people`, wantNK: []string{"name"}},
		{name: "postgres nested block comment", d: pg, text: "select /* a /* b */ from | */ 1", wantNil: true},
		{name: "postgres param is not a dollar quote", d: pg, text: "select * from people where id = $1 and na|", wantNK: []string{"name"}},
		{name: "unknown alias target is skipped", d: sqlite, text: "select * from nope n where n.|", wantNil: true},
		{name: "EXTRACT FROM is not a table", d: pg, text: "select extract(year from created_at), x from people p where p.|", wantNK: []string{"email", "id", "name"}},
		{name: "SUBSTRING FROM is not a table", d: pg, text: "select substring(name from 2) from people where na|", wantNK: []string{"name"}},
		{name: "TRIM FROM is not a table", d: pg, text: "select trim(both 'x' from name) from orders o where o.|", wantNK: []string{"id", "person_id", "total"}},
		{name: "IS DISTINCT FROM is not a table", d: pg, text: "select * from people where name is distinct from email and em|", wantNK: []string{"email"}},
		{name: "IS NOT DISTINCT FROM offers fields", d: pg, text: "select * from people where name is not distinct from |", has: []string{"email", "id"}, hasNot: []string{"orders"}},
		{name: "FROM inside EXTRACT offers fields", d: pg, text: "select extract(year from |) from people", has: []string{"email"}, hasNot: []string{"orders"}},
		{name: "subquery FROM inside EXTRACT is a table", d: pg, text: "select extract(year from (select 1 from orders o where o.|))", wantNK: []string{"id", "person_id", "total"}},
		{name: "number before cursor", d: sqlite, text: "select 12|", wantNil: true},
		{name: "cursor beyond text", d: sqlite, text: "select * from pe|", want: []string{"people"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := at(tc.text)
			got, err := Complete(context.Background(), newFake(), tc.d, req)
			if err != nil {
				t.Fatal(err)
			}
			ls := labels(got)
			switch {
			case tc.wantNil:
				if len(got) != 0 {
					t.Fatalf("want no completions, got %v", ls)
				}
			case tc.wantNK != nil:
				var nk []string
				for _, c := range got {
					if c.Kind != "keyword" {
						nk = append(nk, c.Label)
					}
				}
				if !slices.Equal(nk, tc.wantNK) {
					t.Fatalf("non-keyword labels = %v, want %v", nk, tc.wantNK)
				}
			case tc.want != nil:
				if !slices.Equal(ls, tc.want) {
					t.Fatalf("labels = %v, want %v", ls, tc.want)
				}
			}
			for _, h := range tc.has {
				if !slices.Contains(ls, h) {
					t.Errorf("missing %q in %v", h, ls)
				}
			}
			for _, h := range tc.hasNot {
				if slices.Contains(ls, h) {
					t.Errorf("unexpected %q in %v", h, ls)
				}
			}
			if tc.onlyKnd != "" {
				for _, c := range got {
					if c.Kind != tc.onlyKnd {
						t.Errorf("%q has kind %q, want %q", c.Label, c.Kind, tc.onlyKnd)
					}
				}
			}
		})
	}
}

func TestCompleteCursorClamp(t *testing.T) {
	for _, cur := range []int{-3, 1000} {
		got, err := Complete(context.Background(), newFake(), SQLite(), driver.CompletionRequest{Text: "select * from pe", Cursor: cur})
		if err != nil {
			t.Fatalf("cursor %d: %v", cur, err)
		}
		if cur > 0 && !slices.Equal(labels(got), []string{"people"}) {
			t.Errorf("cursor %d: %v", cur, labels(got))
		}
	}
	// A cursor inside a multi-byte rune must not panic or split it.
	if _, err := Complete(context.Background(), newFake(), SQLite(), driver.CompletionRequest{Text: "select 'é' from p", Cursor: 9}); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteInsertQuoting(t *testing.T) {
	get := func(d Dialect, text string) map[string]driver.Completion {
		t.Helper()
		got, err := Complete(context.Background(), newFake(), d, at(text))
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]driver.Completion{}
		for _, c := range got {
			m[c.Label] = c
		}
		return m
	}
	m := get(SQLite(), "select * from |")
	if got := m["Order Items"].Insert; got != `"Order Items"` {
		t.Errorf("collection with space: Insert = %q", got)
	}
	if got := m["people"].Insert; got != "" {
		t.Errorf("plain collection: Insert = %q, want empty", got)
	}
	if got := m["main"]; got.Kind != "namespace" || got.Insert != "main." {
		t.Errorf("namespace = %+v", got)
	}
	m = get(SQLite(), "select * from \"Order Items\" i where i.|")
	if got := m["order"].Insert; got != `"order"` {
		t.Errorf("reserved field: Insert = %q", got)
	}
	if got := m["Qty"].Insert; got != "" {
		t.Errorf("sqlite is case-insensitive, Qty needs no quotes: Insert = %q", got)
	}
	m = get(Postgres(), "select * from \"Order Items\" i where i.|")
	if got := m["Qty"].Insert; got != `"Qty"` {
		t.Errorf("postgres folds to lower case, Qty must be quoted: Insert = %q", got)
	}
	m = get(SQLite(), "select * from [Order Items] i where i.[s|")
	if got := m["sku"].Insert; got != "[sku]" {
		t.Errorf("bracket prefix keeps bracket quoting: Insert = %q", got)
	}
	m = get(SQLite(), `select * from "pe|`)
	if got := m["people"].Insert; got != `"people"` {
		t.Errorf("quoted prefix always quotes: Insert = %q", got)
	}
	m = get(SQLite(), "select * from |")
	if got := m["main"].Insert; got != "main." {
		t.Errorf("namespace Insert = %q", got)
	}
}

func TestCompleteOrdering(t *testing.T) {
	cat := newFake()
	cat.colls["main"] = []string{"pets", "pe", "people", "Pelican", "per"}
	got, err := Complete(context.Background(), cat, SQLite(), at("select * from pe|"))
	if err != nil {
		t.Fatal(err)
	}
	// The exact match first, then alphabetical ignoring case.
	want := []string{"pe", "Pelican", "people", "per", "pets"}
	if !slices.Equal(labels(got), want) {
		t.Fatalf("labels = %v, want %v", labels(got), want)
	}
}

func TestCompleteKindsAndDetail(t *testing.T) {
	got, err := Complete(context.Background(), newFake(), SQLite(), at("select * from people p, orders o where |"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if c.Label == "id" {
			if c.Kind != "field" || c.Detail != "people, orders" {
				t.Errorf("shared field = %+v, want kind field and detail \"people, orders\"", c)
			}
		}
		if c.Label == "email" && c.Detail != "people" {
			t.Errorf("email detail = %q", c.Detail)
		}
	}
	if k := kinds(got); !k["field"] || !k["keyword"] {
		t.Errorf("kinds = %v", k)
	}
	if n := slices.Index(labels(got), "id"); n < 0 || slices.Contains(labels(got)[n+1:], "id") {
		t.Errorf("want exactly one id in %v", labels(got))
	}
}

func TestCompleteCap(t *testing.T) {
	cat := newFake()
	var many []string
	for i := range 500 {
		many = append(many, fmt.Sprintf("t%03d", i))
	}
	cat.colls["main"] = many
	got, err := Complete(context.Background(), cat, SQLite(), at("select * from |"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != MaxResults || MaxResults != 200 {
		t.Fatalf("len = %d, want 200", len(got))
	}
	if got[0].Label != "main" && got[0].Label != "t000" {
		t.Errorf("first = %q", got[0].Label)
	}
}

func TestCompleteErrors(t *testing.T) {
	cat := newFake()
	cat.err = errors.New("boom")
	if _, err := Complete(context.Background(), cat, SQLite(), at("select * from |")); err == nil {
		t.Fatal("catalog error must propagate")
	}
	if _, err := Complete(context.Background(), cat, SQLite(), at("select * from people p where p.|")); err == nil {
		t.Fatal("fields error must propagate")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Complete(ctx, newFake(), SQLite(), at("select * from |")); err == nil {
		t.Fatal("cancelled context must fail")
	}
}

func TestCompleteNamespaceRequest(t *testing.T) {
	req := at("select * from |")
	req.Namespace = "other"
	got, err := Complete(context.Background(), newFake(), SQLite(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(labels(got), "logs") || slices.Contains(labels(got), "people") {
		t.Errorf("labels = %v", labels(got))
	}
	req = at("select * from logs l where l.|")
	req.Namespace = "other"
	got, _ = Complete(context.Background(), newFake(), SQLite(), req)
	if !slices.Equal(labels(got), []string{"msg"}) {
		t.Errorf("labels = %v", labels(got))
	}
}

func TestTokenStart(t *testing.T) {
	for _, tc := range []struct {
		text string
		want int
	}{
		{"select * from pe|", 14},
		{"select * from |", 14},
		{`select * from "Ord|`, 14},
		{"select p.na| from people p", 9},
		{"select * from people; sel|", 22},
	} {
		req := at(tc.text)
		if got := TokenStart(req.Text, req.Cursor, SQLite()); got != tc.want {
			t.Errorf("%q: TokenStart = %d, want %d", tc.text, got, tc.want)
		}
	}
}

func TestTokenEnd(t *testing.T) {
	for _, tc := range []struct {
		text       string
		start, end int
	}{
		{"select * from pe|ople where x", 14, 20},
		{"select * from people|", 14, 20},
		{"select * from |people", 14, 14}, // cursor at a word start types nothing
		{`select * from "Ord|ers" where x`, 14, 22},
		{`select * from "Ord|ers`, 14, 21},
		{"select * from [Ord|ers] where x", 14, 22},
		{"select * from |", 14, 14},
		{"select p.na|me from people p", 9, 13},
		{"select 'a|bc' from t", 9, 9},
	} {
		req := at(tc.text)
		d := SQLite()
		if s, e := TokenStart(req.Text, req.Cursor, d), TokenEnd(req.Text, req.Cursor, d); s != tc.start || e != tc.end {
			t.Errorf("%q: [%d,%d), want [%d,%d)", tc.text, s, e, tc.start, tc.end)
		}
	}
}

func FuzzComplete(f *testing.F) {
	for _, s := range []string{
		"select * from |", "select p.na| from people p", `select * from "Ord|ers"`, "select 'abc|",
		"/* x |", "-- c|", "select $a$ x |", "select E'a\\'|b'", "select * from [a|", "select * from `a|",
		"select extract(year from |)", "select ((((|", "select * from a.b.c.|", ";;;|;;", "é|é", "select $1|", "|",
	} {
		i := strings.IndexByte(s, '|')
		f.Add(s[:i]+s[i+1:], i)
	}
	f.Add("select * from people", -4)
	f.Add("select * from people", 9999)
	f.Fuzz(func(t *testing.T, text string, cursor int) {
		for _, d := range []Dialect{SQLite(), Postgres(), MySQL(), ClickHouse()} {
			got, err := Complete(context.Background(), newFake(), d, driver.CompletionRequest{Text: text, Cursor: cursor})
			if err != nil {
				t.Fatalf("Complete(%q, %d): %v", text, cursor, err)
			}
			if len(got) > MaxResults {
				t.Fatalf("%d results", len(got))
			}
			cur := clampCursor(text, cursor)
			s, e := TokenStart(text, cursor, d), TokenEnd(text, cursor, d)
			if s > cur || cur > e || e > len(text) || s < 0 {
				t.Fatalf("Complete(%q, %d): token [%d,%d) around cursor %d", text, cursor, s, e, cur)
			}
		}
	})
}

func TestCompleteLongStatementIsFast(t *testing.T) {
	var b strings.Builder
	b.WriteString("select ")
	for range 20000 {
		b.WriteString("extract(year from d), ")
	}
	b.WriteString("x from people p where p.")
	text := b.String()
	start := time.Now()
	got, err := Complete(context.Background(), newFake(), Postgres(), driver.CompletionRequest{Text: text, Cursor: len(text)})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(labels(got), []string{"email", "id", "name"}) {
		t.Fatalf("labels = %v", labels(got))
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("completion took %v on a long statement", d)
	}
	// Ending on a comma after many FROM-bearing calls must be linear as well.
	text = "select " + strings.Repeat("extract(year from d), ", 20000)
	start = time.Now()
	if _, err := Complete(context.Background(), newFake(), Postgres(), driver.CompletionRequest{Text: text, Cursor: len(text)}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("comma completion took %v", d)
	}
}

func TestCompleteMySQL(t *testing.T) {
	d := MySQL()
	tests := []struct {
		name    string
		text    string
		wantNK  []string
		wantNil bool
	}{
		{name: "after from", text: "select * from pe|", wantNK: []string{"people"}},
		{name: "backtick alias", text: "select * from `Order Items` i where i.s|", wantNK: []string{"sku"}},
		{name: "hash comment", text: "select 1 # from |", wantNil: true},
		{name: "after hash comment newline", text: "select 1 # x\nfrom pe|", wantNK: []string{"people"}},
		{name: "double quotes are a string", text: `select * from people where name = "a from |"`, wantNil: true},
		{name: "keyword in double-quoted string is ignored", text: `select * from people where name = "from " and na|`, wantNK: []string{"name"}},
		{name: "backslash escapes a quote", text: `select * from people where name = 'it\'s from ' and na|`, wantNK: []string{"name"}},
		{name: "backslash escapes in double quotes", text: `select * from people where name = "a\" from " and na|`, wantNK: []string{"name"}},
		{name: "dash dash without space is not a comment", text: "select 1--1, na| from people", wantNK: []string{"name"}},
		{name: "dash dash space is a comment", text: "select 1 -- from |", wantNil: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Complete(context.Background(), newFake(), d, at(tc.text))
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantNil {
				if len(got) != 0 {
					t.Fatalf("want no completions, got %v", labels(got))
				}
				return
			}
			var nk []string
			for _, c := range got {
				if c.Kind != "keyword" {
					nk = append(nk, c.Label)
				}
			}
			if !slices.Equal(nk, tc.wantNK) {
				t.Fatalf("non-keyword labels = %v, want %v", nk, tc.wantNK)
			}
		})
	}
}

func TestCompleteMySQLQuotesWithBackticks(t *testing.T) {
	get := func(text string) map[string]driver.Completion {
		t.Helper()
		got, err := Complete(context.Background(), newFake(), MySQL(), at(text))
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]driver.Completion{}
		for _, c := range got {
			m[c.Label] = c
		}
		return m
	}
	m := get("select * from |")
	if got := m["Order Items"].Insert; got != "`Order Items`" {
		t.Errorf("collection with space: Insert = %q, want backticks", got)
	}
	if got := m["people"].Insert; got != "" {
		t.Errorf("plain collection: Insert = %q, want empty", got)
	}
	m = get("select * from `Order Items` i where i.|")
	if got := m["order"].Insert; got != "`order`" {
		t.Errorf("reserved field: Insert = %q, want backticks", got)
	}
	if got := m["Qty"].Insert; got != "" {
		t.Errorf("mysql does not fold case, Qty needs no quotes: Insert = %q", got)
	}
	m = get("select * from `pe|")
	if got := m["people"].Insert; got != "`people`" {
		t.Errorf("backtick prefix keeps backticks: Insert = %q", got)
	}
	cat := newFake()
	cat.colls["main"] = append(cat.colls["main"], "we`ird")
	got, err := Complete(context.Background(), cat, MySQL(), at("select * from we|"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Insert != "`we``ird`" {
		t.Errorf("a backtick in a name is doubled: %+v", got)
	}
}

func TestCompleteClickHouse(t *testing.T) {
	d := ClickHouse()
	tests := []struct {
		name    string
		text    string
		wantNK  []string
		wantNil bool
	}{
		{name: "after from", text: "select * from pe|", wantNK: []string{"people"}},
		{name: "backtick alias", text: "select * from `Order Items` i where i.s|", wantNK: []string{"sku"}},
		{name: "double-quoted alias", text: `select * from "Order Items" i where i.s|`, wantNK: []string{"sku"}},
		{name: "hash comment", text: "select 1 # from |", wantNil: true},
		{name: "nested comment", text: "select /* a /* b */ from */ na| from people", wantNK: []string{"name"}},
		{name: "inside nested comment", text: "select /* a /* b */ from |", wantNil: true},
		{name: "backslash escapes a quote", text: `select * from people where name = 'it\'s from ' and na|`, wantNK: []string{"name"}},
		{name: "backslash escapes in identifiers", text: "select * from `Order\\` Items` i, people p where p.na|", wantNK: []string{"name"}},
		{name: "heredoc is a string", text: "select $$ from |", wantNil: true},
		{name: "dash dash is a comment", text: "select 1 --from |", wantNil: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Complete(context.Background(), newFake(), d, at(tc.text))
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantNil {
				if len(got) != 0 {
					t.Fatalf("want no completions, got %v", labels(got))
				}
				return
			}
			var nk []string
			for _, c := range got {
				if c.Kind != "keyword" {
					nk = append(nk, c.Label)
				}
			}
			if !slices.Equal(nk, tc.wantNK) {
				t.Fatalf("non-keyword labels = %v, want %v", nk, tc.wantNK)
			}
		})
	}
}

func TestCompleteClickHouseQuoting(t *testing.T) {
	cat := newFake()
	cat.colls["main"] = append(cat.colls["main"], "we`ird", `back\slash`)
	get := func(text string) map[string]driver.Completion {
		t.Helper()
		got, err := Complete(context.Background(), cat, ClickHouse(), at(text))
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]driver.Completion{}
		for _, c := range got {
			m[c.Label] = c
		}
		return m
	}
	m := get("select * from |")
	if got := m["Order Items"].Insert; got != "`Order Items`" {
		t.Errorf("collection with space: Insert = %q, want backticks", got)
	}
	if got := m["we`ird"].Insert; got != "`we``ird`" {
		t.Errorf("a backtick in a name is doubled: Insert = %q", got)
	}
	if got := m[`back\slash`].Insert; got != "`back\\\\slash`" {
		t.Errorf("a backslash in a name is escaped: Insert = %q", got)
	}
	if got := m["people"].Insert; got != "" {
		t.Errorf("plain collection: Insert = %q, want empty", got)
	}
	m = get(`select * from "Order Items" i where i.|`)
	if got := m["Qty"].Insert; got != "" {
		t.Errorf("clickhouse does not fold case, Qty needs no quotes: Insert = %q", got)
	}
	m = get(`select * from "pe|`)
	if got := m["people"].Insert; got != `"people"` {
		t.Errorf("double-quote prefix keeps double quotes: Insert = %q", got)
	}
}
