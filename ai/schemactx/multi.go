package schemactx

import (
	"context"
	"fmt"
	"strings"

	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/session"
)

// ConnectionInfo describes one connection of a multi-connection context,
// for the prompts: its name and what a query is for it.
type ConnectionInfo struct {
	Name, Dialect, Kind, Syntax string
}

// BuildAll is Build for a scope that may name several connections (a
// conversation that spans them for cross-source queries). With one
// connection it is Build. With several, every connection gets a section
// headed "connection: <name> (<dialect>, <kind>)" within a shared budget:
// the primary connection, the only one whose collection is in scope, keeps
// half of it and the others share the rest. The returned Context carries
// the primary connection's dialect, kind and syntax and the joined text, and
// its Collections are prefixed with their connection. The infos describe
// every connection in order.
func BuildAll(ctx context.Context, mgr *session.Manager, scope assistant.Scope, budgetTokens int) (Context, []ConnectionInfo, error) {
	conns := scope.Connections()
	if len(conns) <= 1 {
		c, err := Build(ctx, mgr, scope, budgetTokens)
		if err != nil {
			return Context{}, nil, err
		}
		return c, []ConnectionInfo{{Name: scope.Conn, Dialect: c.Dialect, Kind: c.Kind, Syntax: c.Syntax}}, nil
	}
	primaryBudget := budgetTokens / 2
	otherBudget := max((budgetTokens-primaryBudget)/(len(conns)-1), 1)
	var out Context
	var infos []ConnectionInfo
	var sb strings.Builder
	for i, name := range conns {
		s, budget := assistant.Scope{Conn: name}, otherBudget
		if i == 0 {
			s = assistant.Scope{Conn: name, Namespace: scope.Namespace, Collection: scope.Collection}
			budget = primaryBudget
		}
		c, err := Build(ctx, mgr, s, budget)
		if err != nil {
			// Build's errors are already redacted for their connection.
			return Context{}, nil, fmt.Errorf("connection %s: %w", clean(name), err)
		}
		if i == 0 {
			out = Context{Dialect: c.Dialect, Kind: c.Kind, Syntax: c.Syntax}
		} else {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "connection: %s (%s, %s)\n", clean(name), clean(c.Dialect), clean(c.Kind))
		sb.WriteString(c.Text)
		for _, col := range c.Collections {
			out.Collections = append(out.Collections, clean(name)+": "+col)
		}
		infos = append(infos, ConnectionInfo{Name: name, Dialect: c.Dialect, Kind: c.Kind, Syntax: c.Syntax})
	}
	out.Text = sb.String()
	return out, infos, nil
}
