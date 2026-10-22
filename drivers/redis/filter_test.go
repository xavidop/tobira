package redis

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestEscapeGlob(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"user", "user"},
		{"a*b?c[d]e\\f", `a\*b\?c\[d\]e\\f`},
		{"", ""},
	} {
		if got := escapeGlob(tt.in); got != tt.want {
			t.Errorf("escapeGlob(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if ok, _ := globMatch(context.Background(), escapeGlob(tt.in), tt.in); !ok {
			t.Errorf("escaped %q does not match itself", tt.in)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	for _, tt := range []struct {
		pat, s string
		want   bool
	}{
		{"*", "", true}, {"*", "abc", true}, {"a*", "abc", true}, {"a*", "bac", false},
		{"a?c", "abc", true}, {"a?c", "ac", false}, {"*:1", "user:1", true}, {"*:1", "user:12", false},
		{"[ab]x", "bx", true}, {"[ab]x", "cx", false}, {"[^ab]x", "cx", true}, {"[a-c]x", "bx", true},
		{`a\*`, "a*", true}, {`a\*`, "ab", false}, {"h*llo", "heeello", true}, {"**a", "xa", true},
	} {
		if got, err := globMatch(context.Background(), tt.pat, tt.s); err != nil || got != tt.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tt.pat, tt.s, got, tt.want)
		}
	}
}

func TestInCollection(t *testing.T) {
	for _, tt := range []struct {
		coll, key string
		want      bool
	}{
		{"*", "anything", true},
		{"user", "user", false}, // a bare key stays in "*" only
		{"", ":x", false},
		{"user", "user:1", true},
		{"user", "user:1:name", true},
		{"user", "users:1", false},
		{"user", "use", false},
		{"a*b", "a*b:1", true},
		{"a*b", "axb:1", false},
	} {
		if got := inCollection(tt.coll, tt.key); got != tt.want {
			t.Errorf("inCollection(%q, %q) = %v, want %v", tt.coll, tt.key, got, tt.want)
		}
	}
	if got := matchPattern("a*b"); got != `a\*b:*` {
		t.Errorf("matchPattern = %q", got)
	}
	if got := matchPattern("*"); got != "*" {
		t.Errorf("matchPattern(*) = %q", got)
	}
}

func TestCursor(t *testing.T) {
	for _, c := range []browseCursor{{after: "user:20", hasAfter: true}, {sorted: true, offset: 40}} {
		s, err := c.encode()
		if err != nil {
			t.Fatal(err)
		}
		got, err := parseCursor(s)
		if err != nil || got != c {
			t.Errorf("parseCursor(%q) = %+v, %v; want %+v", s, got, err, c)
		}
	}
	if c, err := parseCursor(""); err != nil || c != (browseCursor{}) {
		t.Errorf(`parseCursor("") = %+v, %v`, c, err)
	}
	for _, bad := range []string{"x", "s:1", "s:a:1", "s:1:-1", "o:-1", "o:x", "s:1:2:3", "k:*", "z:1"} {
		if _, err := parseCursor(bad); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("parseCursor(%q) = %v, want ErrSyntax", bad, err)
		}
	}
}

func TestMatchConditions(t *testing.T) {
	cols := []driver.Column{{Name: "_key"}, {Name: "age"}, {Name: "active"}, {Name: "name"}, {Name: "at"}}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	row := driver.Row{driver.String("p:11"), driver.Int(30), driver.Bool(true), driver.String("Ada"), driver.Time(at)}
	nullAge := driver.Row{driver.String("p:2"), driver.Null(), driver.Bool(false), driver.String("Bob"), driver.Null()}
	c := func(f string, op driver.Op, v driver.Value) driver.Condition {
		return driver.Condition{Field: f, Op: op, Value: v}
	}
	for _, tt := range []struct {
		cond driver.Condition
		row  driver.Row
		want bool
	}{
		{c("age", driver.OpEq, driver.Int(30)), row, true},
		{c("age", driver.OpEq, driver.String("30")), row, true},
		{c("age", driver.OpNe, driver.Int(30)), row, false},
		{c("age", driver.OpGt, driver.Int(29)), row, true},
		{c("age", driver.OpGe, driver.Decimal("30.0")), row, true},
		{c("age", driver.OpLt, driver.Int(30)), row, false},
		{c("age", driver.OpLe, driver.Float(30)), row, true},
		{c("age", driver.OpGt, driver.Int(1)), nullAge, false},
		{c("age", driver.OpNe, driver.Int(1)), nullAge, false},
		{c("age", driver.OpIsNull, driver.Null()), nullAge, true},
		{c("age", driver.OpNotNull, driver.Null()), row, true},
		{c("active", driver.OpEq, driver.Bool(true)), row, true},
		{c("active", driver.OpEq, driver.Bool(true)), nullAge, false},
		{c("name", driver.OpContains, driver.String("d")), row, true},
		{c("name", driver.OpPrefix, driver.String("Ad")), row, true},
		{c("name", driver.OpPrefix, driver.String("da")), row, false},
		{c("_key", driver.OpContains, driver.String("1")), row, true},
		{c("age", driver.OpContains, driver.String("3")), row, true},
		{c("name", driver.OpIn, driver.List(driver.String("Bob"), driver.String("Ada"))), row, true},
		{c("name", driver.OpIn, driver.List(driver.String("Bob"))), row, false},
		{c("name", driver.OpLt, driver.String("B")), row, true},
		{c("at", driver.OpGe, driver.Time(at)), row, true},
		{c("at", driver.OpGt, driver.String("2025-12-31T00:00:00Z")), row, true},
		{c("missing", driver.OpIsNull, driver.Null()), row, true},
	} {
		got, err := matchConditions(cols, tt.row, []driver.Condition{tt.cond})
		if err != nil || got != tt.want {
			t.Errorf("%s %s %s on %v = %v, %v; want %v", tt.cond.Field, tt.cond.Op, tt.cond.Value, tt.row, got, err, tt.want)
		}
	}
	if _, err := matchConditions(cols, row, []driver.Condition{c("age", driver.Op("regex"), driver.String("x"))}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("unknown op: %v, want ErrUnsupported", err)
	}
	if _, err := matchConditions(cols, row, []driver.Condition{c("name", driver.OpIn, driver.String("x"))}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("OpIn without a list: %v, want ErrSyntax", err)
	}
}

func TestCompareForSort(t *testing.T) {
	for _, tt := range []struct {
		a, b driver.Value
		want int
	}{
		{driver.Int(1), driver.Int(2), -1},
		{driver.Int(2), driver.Decimal("1.5"), 1},
		{driver.String("a"), driver.String("b"), -1},
		{driver.Bool(false), driver.Bool(true), -1},
		{driver.Int(1), driver.Int(1), 0},
		{driver.Null(), driver.Int(1), 1}, // NULLs sort last
		{driver.Int(1), driver.Null(), -1},
		{driver.Null(), driver.Null(), 0},
	} {
		if got := compareForSort(tt.a, tt.b); got != tt.want {
			t.Errorf("compareForSort(%s, %s) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestIDField(t *testing.T) {
	keys := []string{"p:1", "p:2", "p:10"}
	recs := []map[string]string{
		{"name": "a", "id": "1", "n": "1"},
		{"name": "b", "id": "2", "n": "2"},
		{"name": "c", "id": "10", "n": "x"},
	}
	if got := detectIDField("p", []string{"name", "n", "id"}, keys, recs); got != "id" {
		t.Errorf("detectIDField = %q, want id", got)
	}
	recs[2]["id"] = "11"
	if got := detectIDField("p", []string{"name", "n", "id"}, keys, recs); got != "" {
		t.Errorf("detectIDField with a mismatch = %q, want none", got)
	}
	if got := detectIDField("p", []string{"k"}, []string{"p"}, []map[string]string{{"k": ""}}); got != "" {
		t.Errorf("detectIDField on the bare prefix key = %q, want none", got)
	}
}

// TestGlobLinear is the CVE-2022-36021 shape: many stars against a long
// non-matching key must not backtrack exponentially.
func TestGlobLinear(t *testing.T) {
	pat := strings.Repeat("a*", 40) + "b"
	s := strings.Repeat("a", 20000)
	start := time.Now()
	ok, err := globMatch(context.Background(), pat, s)
	if ok || err != nil {
		t.Errorf("globMatch = %v, %v", ok, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("pathological pattern took %v", d)
	}
	for _, tt := range []struct {
		pat, s string
		want   bool
	}{
		{"*a*b*c*", "xxaxxbxxcxx", true}, {"*a*b*c*", "xxaxxcxxbxx", false}, {"a*?", "a", false},
		{"a*?", "ab", true}, {"*[0-9]", "user:7", true}, {"*\\*", "a*", true}, {"*\\*", "ab", false},
	} {
		if got, _ := globMatch(context.Background(), tt.pat, tt.s); got != tt.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tt.pat, tt.s, got, tt.want)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := globMatch(ctx, pat, strings.Repeat("a", 1<<20)); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled match = %v, want context.Canceled", err)
	}
}
