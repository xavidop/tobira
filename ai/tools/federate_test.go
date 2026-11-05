package tools

import (
	"strings"
	"testing"

	"github.com/xavidop/tobira/internal/federate"
)

const twoSourceStatement = "-- @source a = dev: select * from orders\n-- @source b = dev2: select * from orders\nselect count(*) from a join b on a.id = b.id"

// federate is offered only to a turn that may name at least two readable
// connections; it is not one of the read tools.
func TestFederateOfferedWithSeveralConnections(t *testing.T) {
	e := newEnv(t)
	if e.ts.FederateTool().Name() != NameFederate {
		t.Fatalf("FederateTool() = %q", e.ts.FederateTool().Name())
	}
	for _, r := range e.ts.Read() {
		if r.Name() == NameFederate {
			t.Fatal("federate is among the read tools")
		}
	}
	if !e.ts.Federatable(ctx) {
		t.Error("every connection allowed: want federatable")
	}
	if !e.ts.Federatable(WithConnections(ctx, "dev", "dev2")) {
		t.Error("two connections allowed: want federatable")
	}
	if e.ts.Federatable(WithConnections(ctx, "dev")) {
		t.Error("one connection allowed: want not federatable")
	}
	if e.ts.Federatable(WithConnections(ctx, "dev", "bad")) {
		t.Error("one connection and a broken one: want not federatable")
	}
}

// A statement naming a connection outside the turn is refused as unknown and
// one naming a broken connection as broken, before anything runs; so are a
// @sink and text that is not a cross-source statement.
func TestFederateRefusals(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name, stmt string
		ctxConns   []string
		want       string
	}{
		{"outside the scope", twoSourceStatement, []string{"dev"}, `unknown connection "dev2"`},
		{"not configured", "-- @source a = nope: select 1\nselect * from a", nil, `unknown connection "nope"`},
		{"broken", "-- @source a = bad: select 1\nselect * from a", nil, `connection "bad" is broken`},
		{"sink", twoSourceStatement + "\n-- @sink dev: t", nil, "@sink is not supported"},
		{"plain query", "select 1", nil, "not a cross-source statement"},
		{"empty", "  ", nil, "statement is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ctx
			if tc.ctxConns != nil {
				c = WithConnections(ctx, tc.ctxConns...)
			}
			out, err := e.ts.Federate(c, FederateInput{Statement: tc.stmt})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.Error, tc.want) {
				t.Errorf("error = %q, want it to contain %q", out.Error, tc.want)
			}
		})
	}
	for _, op := range e.fakeOps() {
		if op == "query" {
			t.Error("a refused statement reached the driver")
		}
	}
	if len(e.hist.all()) != 0 {
		t.Errorf("refusals were recorded: %+v", e.hist.all())
	}
}

// The tool reports its call with no connection of its own; the statement
// is its input.
func TestFederateReportsTheStatement(t *testing.T) {
	e := newEnv(t)
	if _, err := e.ts.Federate(ctx, FederateInput{Statement: "select 1"}); err != nil {
		t.Fatal(err)
	}
	calls := e.toolCalls()
	if len(calls) != 1 || calls[0].Name != NameFederate || calls[0].Conn != "" || !strings.Contains(calls[0].Input, "select 1") || calls[0].Rows != -1 {
		t.Fatalf("tool calls = %+v", calls)
	}
}

// The history connection of a cross-source run is the engine's.
func TestFederateHistoryConn(t *testing.T) {
	if federate.HistoryConn != "federated" {
		t.Fatalf("federate.HistoryConn = %q", federate.HistoryConn)
	}
}
