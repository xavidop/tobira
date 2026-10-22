package schemactx

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivers/sqlite"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var ctx = context.Background()

const dsnPassword = "s3cretpw"

// newFake returns a Fake with People, orders (-> people), items (-> orders)
// and two unrelated collections, and a Manager with connection "dev".
func newFake(t *testing.T, opts ...drivertest.FakeOption) (*drivertest.Fake, *session.Manager) {
	t.Helper()
	f := drivertest.NewFake(append([]drivertest.FakeOption{drivertest.WithPeople()}, opts...)...)
	f.AddCollection("main", "orders",
		[]driver.Column{{Name: "id", Type: driver.TypeInt}, {Name: "person_id", Type: driver.TypeInt}, {Name: "status", Type: driver.TypeString, NativeType: "text"}},
		[]driver.Row{{driver.Int(1), driver.Int(3), driver.String("open")}}, "id")
	f.OnDescribe("main", "orders", func(s *driver.CollectionSchema) {
		s.Comment = "customer orders; token " + dsnPassword
		s.DDL = "CREATE TABLE orders (secret DEFAULT '" + dsnPassword + "')"
		s.ForeignKeys = []driver.ForeignKey{{Name: "orders_person", Fields: []string{"person_id"}, References: driver.CollectionRef{Name: "tobira_people"}, RefFields: []string{"id"}}}
		s.Indexes = append(s.Indexes, driver.Index{Name: "orders_status", Fields: []string{"status"}})
		for i := range s.Fields {
			if s.Fields[i].Name == "status" {
				s.Fields[i].Default = "'open'"
				s.Fields[i].Comment = "order state\nopen or paid"
			}
		}
	})
	f.AddCollection("main", "items",
		[]driver.Column{{Name: "id", Type: driver.TypeInt}, {Name: "order_id", Type: driver.TypeInt}, {Name: "sku", Type: driver.TypeString}}, nil, "id")
	f.OnDescribe("main", "items", func(s *driver.CollectionSchema) {
		s.ForeignKeys = []driver.ForeignKey{{Fields: []string{"order_id"}, References: driver.CollectionRef{Namespace: "main", Name: "orders"}, RefFields: []string{"id"}}}
	})
	f.AddCollection("main", "audit", []driver.Column{{Name: "at", Type: driver.TypeTime}}, nil)
	f.AddCollection("main", "zeta", []driver.Column{{Name: "z", Type: driver.TypeInt}}, nil)
	reg := driver.NewRegistry()
	if err := reg.Add(f.Driver()); err != nil {
		t.Fatal(err)
	}
	mgr := session.New(&config.Config{Connections: []config.Connection{{Name: "dev", Driver: "fake", DSN: "fake://admin:" + dsnPassword + "@db/app"}}}, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	return f, mgr
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal([]byte(got), want) {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func build(t *testing.T, mgr *session.Manager, scope assistant.Scope, budget int) Context {
	t.Helper()
	c, err := Build(ctx, mgr, scope, budget)
	if err != nil {
		t.Fatal(err)
	}
	if utf8Safe := strings.ToValidUTF8(c.Text, "?"); utf8Safe != c.Text {
		t.Fatal("text is not valid UTF-8")
	}
	return c
}

func TestScopedWithNeighbours(t *testing.T) {
	_, mgr := newFake(t)
	c := build(t, mgr, assistant.Scope{Conn: "dev", Namespace: "main", Collection: "orders"}, 8000)
	if c.Dialect != "fake" || c.Kind != "other" {
		t.Fatalf("dialect/kind = %q/%q", c.Dialect, c.Kind)
	}
	if !slices.Equal(c.Collections, []string{"main.orders", "main.tobira_people", "main.items"}) {
		t.Fatalf("collections = %v", c.Collections)
	}
	if strings.Contains(c.Text, dsnPassword) || strings.Contains(c.Text, "CREATE TABLE") {
		t.Fatalf("text leaks the DDL or a secret:\n%s", c.Text)
	}
	golden(t, "scoped", c.Text)
	again := build(t, mgr, assistant.Scope{Conn: "dev", Namespace: "main", Collection: "orders"}, 8000)
	if again.Text != c.Text {
		t.Fatal("Build is not deterministic")
	}
}

// The default namespace is used when the scope has none.
func TestDefaultNamespace(t *testing.T) {
	_, mgr := newFake(t)
	a := build(t, mgr, assistant.Scope{Conn: "dev", Collection: "orders"}, 8000)
	b := build(t, mgr, assistant.Scope{Conn: "dev", Namespace: "main", Collection: "orders"}, 8000)
	if a.Text != b.Text {
		t.Fatalf("default namespace differs:\n%s\n---\n%s", a.Text, b.Text)
	}
}

// Over budget, the tail goes first: the bare names, then the neighbours from
// the last, then the scoped collection's comments. The scoped collection
// itself always stays.
func TestBudget(t *testing.T) {
	f, mgr := newFake(t)
	for i := range 10 {
		f.AddCollection("main", fmt.Sprintf("extra_%02d", i), []driver.Column{{Name: "x", Type: driver.TypeInt}}, nil)
	}
	scope := assistant.Scope{Conn: "dev", Namespace: "main", Collection: "orders"}
	full := build(t, mgr, scope, 8000)
	fullTokens := tokens(full.Text)

	prev := full
	var steps []string
	for budget := fullTokens; budget > 0; budget -= 5 {
		c := build(t, mgr, scope, budget)
		if tokens(c.Text) > budget && len(c.Collections) != 1 {
			t.Fatalf("budget %d: %d tokens with %v; only the scoped collection alone may overflow", budget, tokens(c.Text), c.Collections)
		}
		if !strings.Contains(c.Text, "- name: main.orders") {
			t.Fatalf("budget %d: scoped collection missing:\n%s", budget, c.Text)
		}
		if c.Text != prev.Text {
			steps = append(steps, fmt.Sprintf("budget %d: %d tokens, collections %v", budget, tokens(c.Text), c.Collections))
			// Once something is dropped it never comes back at a smaller budget.
			for _, name := range c.Collections {
				if !slices.Contains(prev.Collections, name) {
					t.Fatalf("budget %d brought back %s", budget, name)
				}
			}
		}
		prev = c
	}
	if len(steps) < 3 {
		t.Fatalf("expected several degradation steps, got %v", steps)
	}
	// Each stage at a chosen budget, as goldens.
	trimmed := build(t, mgr, scope, fullTokens-5)
	if len(trimmed.Collections) != 3 || !strings.Contains(trimmed.Text, " more)") {
		t.Fatalf("a small cut must trim names, not neighbours:\n%s", trimmed.Text)
	}
	golden(t, "budget_names_trimmed", trimmed.Text)
	golden(t, "budget_one_neighbour", build(t, mgr, scope, tokens(withoutLast(t, mgr, scope))).Text)
	golden(t, "budget_tiny", build(t, mgr, scope, 1).Text)
}

// withoutLast is the text at the largest budget that drops the last
// neighbour (items).
func withoutLast(t *testing.T, mgr *session.Manager, scope assistant.Scope) string {
	t.Helper()
	for b := tokens(build(t, mgr, scope, 8000).Text); b > 0; b-- {
		c := build(t, mgr, scope, b)
		if !slices.Contains(c.Collections, "main.items") {
			return c.Text
		}
	}
	t.Fatal("items never dropped")
	return ""
}

func tokens(s string) int { return (len(s) + 3) / 4 }

func TestUnscoped(t *testing.T) {
	f, mgr := newFake(t)
	for i := range 4 {
		f.AddCollection("main", fmt.Sprintf("extra_%d", i), []driver.Column{{Name: "x", Type: driver.TypeInt}}, nil)
	}
	c := build(t, mgr, assistant.Scope{Conn: "dev"}, 8000)
	if len(c.Collections) != 5 {
		t.Fatalf("described %v, want 5", c.Collections)
	}
	golden(t, "unscoped", c.Text)
	// Over budget, describes go first and the name list stays.
	small := build(t, mgr, assistant.Scope{Conn: "dev"}, 60)
	if len(small.Collections) >= 5 || !strings.Contains(small.Text, "extra_3") {
		t.Fatalf("unscoped small budget:\n%s\n%v", small.Text, small.Collections)
	}
}

// The incoming-FK scan stops after maxDescribes Describes and after the
// scan timeout, whichever comes first.
func TestIncomingScanBounds(t *testing.T) {
	f, mgr := newFake(t)
	for i := range 60 {
		f.AddCollection("main", fmt.Sprintf("t%02d", i), []driver.Column{{Name: "x", Type: driver.TypeInt}}, nil)
	}
	f.AddCollection("main", "late_ref", []driver.Column{{Name: "order_id", Type: driver.TypeInt}}, nil)
	f.OnDescribe("main", "late_ref", func(s *driver.CollectionSchema) {
		s.ForeignKeys = []driver.ForeignKey{{Fields: []string{"order_id"}, References: driver.CollectionRef{Name: "orders"}}}
	})
	c := build(t, mgr, assistant.Scope{Conn: "dev", Collection: "orders"}, 100000)
	n := 0
	for _, call := range f.Calls() {
		if call.Op == "describe" {
			n++
		}
	}
	if n > maxDescribes {
		t.Fatalf("describes = %d, want at most %d", n, maxDescribes)
	}
	if slices.Contains(c.Collections, "main.late_ref") {
		t.Fatal("the scan went past its Describe bound")
	}
	if !slices.Contains(c.Collections, "main.items") {
		t.Fatal("an early incoming FK was missed")
	}

	f2, mgr2 := newFake(t)
	for i := range 20 {
		f2.AddCollection("main", fmt.Sprintf("t%02d", i), []driver.Column{{Name: "x", Type: driver.TypeInt}}, nil)
	}
	f2.SetLatency(30 * time.Millisecond)
	start := time.Now()
	out, err := build2(mgr2, assistant.Scope{Conn: "dev", Collection: "orders"}, 8000, limits{describes: maxDescribes, scan: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second || !strings.Contains(out.Text, "main.orders") {
		t.Fatalf("scan timeout not honoured: %v\n%s", time.Since(start), out.Text)
	}
	n = 0
	for _, call := range f2.Calls() {
		if call.Op == "describe" {
			n++
		}
	}
	if n > 8 {
		t.Fatalf("the scan kept describing after its timeout: %d describes", n)
	}
}

func build2(mgr *session.Manager, scope assistant.Scope, budget int, l limits) (Context, error) {
	return buildWith(ctx, mgr, scope, budget, l)
}

func TestErrors(t *testing.T) {
	_, mgr := newFake(t)
	if _, err := Build(ctx, mgr, assistant.Scope{Conn: "nope"}, 8000); err == nil {
		t.Fatal("unknown connection: no error")
	}
	_, err := Build(ctx, mgr, assistant.Scope{Conn: "dev", Collection: "nope"}, 8000)
	if err == nil || strings.Contains(err.Error(), dsnPassword) {
		t.Fatalf("unknown collection: %v", err)
	}
}

func TestSQLiteNeighbours(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	c, err := sqlite.New().Open(ctx, driver.ConnConfig{DSN: "sqlite:" + path})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range append(drivertest.SQLSeed("sqlite"),
		"create table orders (id integer primary key, person_id integer not null references tobira_people(id), status text default 'open')",
		"create table items (id integer primary key, order_id integer references orders(id), sku text)",
		"create table audit (at text)") {
		rs, err := c.Query(ctx, driver.Query{Text: s})
		if err != nil {
			t.Fatal(err)
		}
		_ = rs.Close()
	}
	_ = c.Close()
	reg := driver.NewRegistry()
	if err := reg.Add(sqlite.New()); err != nil {
		t.Fatal(err)
	}
	mgr := session.New(&config.Config{Connections: []config.Connection{{Name: "lite", Driver: "sqlite", DSN: "sqlite:" + path}}}, reg, secrets.Resolver{}, false)
	defer func() { _ = mgr.CloseAll() }()
	got := build(t, mgr, assistant.Scope{Conn: "lite", Collection: "orders"}, 8000)
	if got.Dialect != "sqlite" || got.Kind != "sql" {
		t.Fatalf("dialect/kind = %s/%s", got.Dialect, got.Kind)
	}
	if !slices.Equal(got.Collections, []string{"main.orders", "main.tobira_people", "main.items"}) {
		t.Fatalf("collections = %v\n%s", got.Collections, got.Text)
	}
	if strings.Contains(strings.ToLower(got.Text), "create table") {
		t.Fatal("DDL in the context")
	}
	golden(t, "sqlite", got.Text)
}

// Comments are redacted before they are cut, at either cut length, so a
// password straddling the cut never survives as a prefix.
func TestCommentRedactedBeforeCut(t *testing.T) {
	f, mgr := newFake(t)
	scope := assistant.Scope{Conn: "dev", Namespace: "main", Collection: "orders"}
	f.OnDescribe("main", "orders", func(s *driver.CollectionSchema) {
		s.Comment = strings.Repeat("c", commentRunes-3) + dsnPassword + strings.Repeat("d", 50)
		s.Fields[2].Comment = strings.Repeat("e", 62) + dsnPassword + strings.Repeat("f", 300)
	})
	full := build(t, mgr, scope, 8000)
	for b := tokens(full.Text); b > 0; b -= 3 {
		if c := build(t, mgr, scope, b); strings.Contains(c.Text, dsnPassword[:3]) {
			t.Fatalf("budget %d leaves a password prefix:\n%s", b, c.Text)
		}
	}
}

// The header goes through the redactor like everything else.
func TestHeaderIsRedacted(t *testing.T) {
	f, mgr := newFake(t)
	f.AddCollection(dsnPassword, "t", []driver.Column{{Name: "x", Type: driver.TypeInt}}, nil)
	c := build(t, mgr, assistant.Scope{Conn: "dev", Namespace: dsnPassword}, 8000)
	if strings.Contains(c.Text, dsnPassword) {
		t.Fatalf("header leaks:\n%s", c.Text)
	}
}

// A wide-column table's partition and clustering keys reach the model, in
// clustering order; a search index's mapping shows as nested fields.
func TestWideColumnAndSearchSections(t *testing.T) {
	_, mgr := newFake(t, drivertest.WithWideColumns(), drivertest.WithSearch(), drivertest.WithKind(driver.KindWideColumn))
	c := build(t, mgr, assistant.Scope{Conn: "dev", Namespace: "main", Collection: drivertest.WideColumnCollection}, 8000)
	if c.Kind != "widecolumn" {
		t.Fatalf("kind = %q", c.Kind)
	}
	for _, want := range []string{
		"    partition_key: tenant\n",
		"    clustering_key: ts desc, seq asc\n",
		"      - tenant string pk\n",
	} {
		if !strings.Contains(c.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, c.Text)
		}
	}
	s := build(t, mgr, assistant.Scope{Conn: "dev", Namespace: "main", Collection: drivertest.SearchCollection}, 8000)
	for _, want := range []string{"    type: index\n", "      - author object\n", "        - name string\n"} {
		if !strings.Contains(s.Text, want) {
			t.Errorf("search text lacks %q:\n%s", want, s.Text)
		}
	}
	if strings.Contains(s.Text, "partition_key") || strings.Contains(s.Text, "clustering_key") {
		t.Errorf("an index has no partition or clustering key:\n%s", s.Text)
	}
}
