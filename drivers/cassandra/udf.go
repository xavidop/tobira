package cassandra

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// udfTTL is how long the names of the cluster's user-defined functions
// and aggregates are reused by the read-only guard.
const udfTTL = 60 * time.Second

// udfCache remembers the lower-cased names of every user-defined function
// and aggregate in the cluster. It is safe for concurrent use.
type udfCache struct {
	mu    sync.Mutex
	gen   uint64
	at    time.Time
	names map[string]bool
}

func (u *udfCache) invalidate() {
	u.mu.Lock()
	u.gen++
	u.names = nil
	u.mu.Unlock()
}

// userFunctions returns the cached names, reading system_schema.functions
// and system_schema.aggregates (all keyspaces: an unqualified call
// resolves in the statement's keyspace, which a qualified table name can
// change) when they are older than udfTTL.
func (c *conn) userFunctions(ctx context.Context) (map[string]bool, error) {
	u := &c.udfs
	u.mu.Lock()
	if u.names != nil && time.Since(u.at) < udfTTL {
		names := u.names
		u.mu.Unlock()
		return names, nil
	}
	gen := u.gen
	u.mu.Unlock()
	names := map[string]bool{}
	for _, q := range []string{
		"SELECT function_name FROM system_schema.functions",
		"SELECT aggregate_name FROM system_schema.aggregates",
	} {
		iter := c.session.Query(q).Idempotent(true).IterContext(ctx)
		var name string
		for iter.Scan(&name) {
			names[strings.ToLower(name)] = true
		}
		if err := iter.Close(); err != nil {
			return nil, err
		}
	}
	u.mu.Lock()
	if u.gen == gen {
		u.names, u.at = names, time.Now()
	}
	u.mu.Unlock()
	return names, nil
}

// guard is the read-only path for a statement: the lexical allow-list
// (checkReadOnly), then, when the statement calls built-in names
// unqualified, a check that no user-defined function or aggregate shares
// one of them. It fails closed: when the function catalog cannot be read
// the statement is refused.
func (c *conn) guard(ctx context.Context, text string) error {
	if err := checkReadOnly(text); err != nil {
		return err
	}
	calls := builtinCalls(text)
	if len(calls) == 0 {
		return nil
	}
	udfs, err := c.userFunctions(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return mapError(ctx, "query", err)
		}
		return readOnlyError(errors.New("cannot check the statement for user-defined functions: system_schema.functions is unreadable; call built-ins as system.name()"))
	}
	return refuseShadowed(calls, udfs)
}
