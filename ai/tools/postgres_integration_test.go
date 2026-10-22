//go:build integration

package tools

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivers/postgres"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
)

// A cast error quotes the row value ("invalid input syntax for type
// integer: "alice@example.com""). Under send_rows: never the tool must not
// pass it to the model; under always it may.
func TestPostgresCastErrorFollowsTheRowPolicy(t *testing.T) {
	bg := context.Background()
	ctr, err := tcpostgres.Run(bg, "postgres:17",
		tcpostgres.WithDatabase("tobira"), tcpostgres.WithUsername("tobira"), tcpostgres.WithPassword("tobira"),
		tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })
	dsn, err := ctr.ConnectionString(bg, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	c, err := postgres.New().Open(bg, driver.ConnConfig{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"create table people (id integer primary key, email text, payload text)",
		`insert into people values (1, 'alice@example.com', '{"ssn":"123-45-6789"}'), (2, 'bob.12345@example.com', '{"ssn":"987-65-4321"}')`,
	} {
		rs, err := c.Query(bg, driver.Query{Text: s})
		if err != nil {
			t.Fatal(err)
		}
		_ = rs.Close()
	}
	_ = c.Close()

	reg := driver.NewRegistry()
	if err := reg.Add(postgres.New()); err != nil {
		t.Fatal(err)
	}
	mgr := session.New(&config.Config{Connections: []config.Connection{{Name: "pg", Driver: "postgres", DSN: dsn}}}, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })

	leaky := []string{"alice", "bob", "example", "ssn", "123-45", "6789", "987-65", "4321", "does not exist", "7b22"}
	// reduced is the only shape an error may have under never, bar syntax errors.
	reduced := regexp.MustCompile(`^[a-z -]+: [a-z;/ -]+ \(SQLSTATE [0-9A-Z]{5}\)$`)
	for _, q := range []string{
		"select email::int from people",
		"select email::int from people offset 1",
		"select payload::int from people",
		`select ('"' || email)::int from people`,
		"select payload::json->>'ssn' || 1 from people where (payload::json->>'ssn')::int > 0",
		// Casts whose error echoes the value unquoted (SQLSTATE 42883), plain and encoded.
		"select email::regoper from people",
		"select email::regoperator from people",
		"select translate(encode(convert_to(payload, 'UTF8'), 'hex'), '0123456789', 'ghijklmnop')::regoper from people",
		"select query_to_xml('select ' || split_part(email, '@', 1) || '()', true, false, '') from people",
	} {
		never, _ := NewToolset(ToolDeps{Manager: mgr, Policy: RowsNever}).Query(ctx, QueryInput{Connection: "pg", Query: q})
		if !reduced.MatchString(never.Error) {
			t.Errorf("never, %q: want kind and code only, got %q", q, never.Error)
		}
		for _, l := range leaky {
			if strings.Contains(never.Error, l) {
				t.Errorf("never, %q: error leaks %q: %q", q, l, never.Error)
			}
		}
		always, _ := NewToolset(ToolDeps{Manager: mgr, Policy: RowsAlways}).Query(ctx, QueryInput{Connection: "pg", Query: q})
		if !strings.Contains(always.Error, "SQLSTATE") || always.Error == never.Error {
			t.Errorf("always, %q: want the full error, got %q", q, always.Error)
		}
	}
	// With rows allowed the echo comes back in full: the leak was real.
	always, _ := NewToolset(ToolDeps{Manager: mgr, Policy: RowsAlways}).Query(ctx, QueryInput{Connection: "pg", Query: "select email::regoper from people"})
	if !strings.Contains(always.Error, "alice@example.com") {
		t.Errorf("always: want the full error, got %q", always.Error)
	}
	// A syntax error keeps the text before the first quote, and its code.
	syn, _ := NewToolset(ToolDeps{Manager: mgr, Policy: RowsNever}).Query(ctx, QueryInput{Connection: "pg", Query: "select from from people"})
	if syn.Error != "invalid query: ERROR: syntax error at or near … (SQLSTATE 42601)" {
		t.Errorf("syntax error under never: %q", syn.Error)
	}
}
