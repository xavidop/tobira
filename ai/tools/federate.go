package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/federate"
)

// FederateInput is the input of federate.
type FederateInput struct {
	Statement string `json:"statement" jsonschema:"description=A cross-source statement: one '-- @source <alias> = <connection>[/<namespace>]: <native query>' line per source (continued on following -- lines), then one read-only SQLite statement over the alias tables"`
	Limit     int    `json:"limit,omitempty" jsonschema:"description=Rows of the final result to return; capped at the configured maximum"`
}

// FederateTool returns the federate tool. The assistant and the MCP server
// offer it only when Federatable.
func (t *Toolset) FederateTool() ai.ToolRef { return t.federate }

// Federatable reports whether ctx's turn may name at least two connections
// whose drivers guarantee read-only queries, so a cross-source statement
// has something to join. It opens nothing.
func (t *Toolset) Federatable(ctx context.Context) bool {
	if t.deps.Manager == nil {
		return false
	}
	n := 0
	for _, name := range t.deps.Manager.Names() {
		if t.sourceRefusal(ctx, name) == nil {
			n++
		}
	}
	return n >= 2
}

// sourceRefusal says why name cannot be a source of a cross-source
// statement in ctx's turn, nil when it can: it must be a configured
// connection the turn may use, available, not broken, and its driver must
// declare the read-only guard, as for every read tool.
func (t *Toolset) sourceRefusal(ctx context.Context, name string) error {
	if !t.allowed(ctx, name) || !slices.Contains(t.deps.Manager.Names(), name) {
		return errUnknownConn(name)
	}
	if why, ok := t.deps.Unavailable[name]; ok {
		return errUnavailable(name, why)
	}
	st := t.deps.Manager.State(name)
	if st.Broken {
		return fmt.Errorf("connection %q is broken: %s", name, firstLine(st.Err))
	}
	if !driver.HasCapability(st.Info, driver.CapReadOnly) {
		return fmt.Errorf("connection %q: its driver does not guarantee read-only queries, so it cannot be a source", name)
	}
	return nil
}

// Federate runs a cross-source statement read-only: every source on its
// own connection, the final statement in a scratch SQLite database. Every
// source connection must pass sourceRefusal; a statement with a @sink is
// refused. Each staged query is recorded on its connection and the whole
// statement under federate.HistoryConn. The rows obey the row policy for
// every source connection: under ask, each is asked once, and one refusal
// withholds them all.
func (t *Toolset) Federate(ctx context.Context, in FederateInput) (RowsOutput, error) {
	return run(t, ctx, NameFederate, "", in, func(call, turn context.Context) (RowsOutput, error) {
		if strings.TrimSpace(in.Statement) == "" {
			return RowsOutput{}, errors.New("statement is empty")
		}
		st, err := federate.Parse(in.Statement)
		if err != nil {
			return RowsOutput{}, fmt.Errorf("not a cross-source statement: %w", err)
		}
		if st.Sink != nil {
			return RowsOutput{}, errors.New("@sink is not supported here: the federate tool only reads")
		}
		var conns []string
		for _, s := range st.Sources {
			if err := t.sourceRefusal(call, s.Conn); err != nil {
				return RowsOutput{}, err
			}
			if !slices.Contains(conns, s.Conn) {
				conns = append(conns, s.Conn)
			}
		}
		lim := t.limit(in.Limit)
		start := time.Now()
		res, err := federate.Run(call, t.deps.Manager, st, federate.Options{
			MaxSourceRows: t.deps.MaxSourceRows, Limit: lim, Registry: t.deps.Registry, Allowed: conns,
		})
		if res != nil {
			for _, r := range res.Sources {
				if r.Err != nil && r.Took == 0 {
					continue // never started: cancelled because another source failed
				}
				t.record(call, r.Conn, r.Query, r.Err == nil, int64(r.Rows), r.Took)
			}
		}
		text := in.Statement
		for _, c := range conns {
			text = t.redact(c, text)
		}
		var n int64
		if err == nil {
			n = int64(len(res.Rows))
		}
		t.record(call, federate.HistoryConn, text, err == nil, n, time.Since(start))
		if err != nil {
			// The engine masks each source's message with its connection; the
			// error may still quote row values, so it obeys the row policy.
			return RowsOutput{}, rowData(err)
		}
		out := RowsOutput{Truncated: res.Stats.Truncated, count: len(res.Rows), Columns: t.columns("", res.Columns)}
		for _, w := range res.Stats.Warnings {
			out.Warnings = append(out.Warnings, truncate(t.redact("", w), MaxValueRunes))
		}
		what := t.consentWhat("", NameFederate, in.Statement)
		send := true
		var note string
		for _, c := range conns {
			ok, why, err := t.allowRows(turn, c, len(res.Rows), what)
			if err != nil {
				return RowsOutput{}, err
			}
			if !ok {
				send, note = false, why
				break
			}
		}
		if !send {
			out.RowsWithheld, out.Note = len(res.Rows), note
			return out, nil
		}
		// The rows come from every source: mask them with each connection's
		// settings in turn.
		rows := string(t.encodeRows("", res.Rows))
		for _, c := range conns {
			rows = t.redact(c, rows)
		}
		out.Rows = json.RawMessage(rows)
		return out, nil
	})
}
