package cassandra

import (
	"context"
	"strings"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

// Complete suggests CQL keywords, keyspaces, tables and columns for the
// query text around the cursor. Catalog reads go through Namespaces,
// Collections and Describe (system_schema) and are cached for
// sqlcomplete.DefaultTTL; Invalidate drops the cache. Names that need
// quoting (upper case, keywords) are inserted in double quotes.
func (c *conn) Complete(ctx context.Context, req driver.CompletionRequest) ([]driver.Completion, error) {
	if err := c.check(ctx, "complete"); err != nil {
		return nil, err
	}
	return sqlcomplete.Complete(ctx, c.catalog, cqlDialect(), req)
}

// Invalidate drops the cached catalog, the cached Describe answers and
// the guard's list of user functions, so the next call sees the schema as
// it is now.
func (c *conn) Invalidate() {
	c.catalog.Invalidate()
	c.schemas.invalidate()
	c.udfs.invalidate()
}

// cqlKeywords are offered in keyword contexts.
const cqlKeywords = `SELECT FROM WHERE AND IN CONTAINS KEY LIKE IS NOT NULL ORDER BY ASC DESC GROUP LIMIT PER PARTITION
ALLOW FILTERING DISTINCT JSON AS CAST TOKEN TTL WRITETIME MAXWRITETIME COUNT MIN MAX SUM AVG NOW TOJSON FROMJSON
INSERT INTO VALUES IF EXISTS UPDATE SET DELETE USING TIMESTAMP BEGIN UNLOGGED COUNTER BATCH APPLY TRUNCATE
CREATE ALTER DROP KEYSPACE TABLE INDEX CUSTOM MATERIALIZED VIEW TYPE FUNCTION AGGREGATE PRIMARY WITH CLUSTERING
REPLICATION DESCRIBE DESC KEYSPACES TABLES TYPES FUNCTIONS GRANT REVOKE ROLE USER ON TO ANN OF TRUE FALSE
FROZEN LIST MAP TUPLE VECTOR`

// cqlReserved are CQL's reserved keywords, which must be quoted as names.
const cqlReserved = `ADD ALLOW ALTER AND APPLY ASC AUTHORIZE BATCH BEGIN BY COLUMNFAMILY CREATE DELETE DESC DESCRIBE DROP
ENTRIES EXECUTE FROM FULL GRANT IF IN INDEX INFINITY INSERT INTO IS KEYSPACE LIMIT MATERIALIZED MBEAN MBEANS MODIFY
NAN NORECURSIVE NOT NULL OF ON OR ORDER PRIMARY RENAME REPLACE REVOKE SCHEMA SELECT SET TABLE TO TOKEN TRUNCATE
UNLOGGED UPDATE USE USING VIEW WHERE WITH`

// cqlDialect describes CQL to the completion engine: unquoted names fold
// to lower case, "…" quotes them, and $$…$$ is a string. (The engine's
// PostgreSQL rules also read $tag$ strings and nested comments, which CQL
// lacks; that only affects completion, never the read-only guard.)
func cqlDialect() sqlcomplete.Dialect {
	return sqlcomplete.Dialect{
		Name:       "cql",
		Keywords:   strings.Fields(cqlKeywords),
		Reserved:   strings.Fields(cqlReserved),
		LowerFold:  true,
		PGSyntax:   true,
		IdentQuote: '"',
	}
}

// catalogReader adapts the connection's catalog to sqlcomplete.Catalog.
type catalogReader struct{ c *conn }

func (r catalogReader) Namespaces(ctx context.Context) ([]string, error) {
	nss, err := r.c.Namespaces(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(nss))
	for i, n := range nss {
		out[i] = n.Name
	}
	return out, nil
}

func (r catalogReader) Collections(ctx context.Context, ns string) ([]string, error) {
	colls, err := r.c.Collections(ctx, driver.NamespaceRef{Name: ns})
	if err != nil {
		return nil, err
	}
	out := make([]string, len(colls))
	for i, c := range colls {
		out[i] = c.Ref.Name
	}
	return out, nil
}

func (r catalogReader) Fields(ctx context.Context, ns, coll string) ([]string, error) {
	s, err := r.c.Describe(ctx, driver.CollectionRef{Namespace: ns, Name: coll})
	if err != nil {
		return nil, err
	}
	out := make([]string, len(s.Fields))
	for i, f := range s.Fields {
		out[i] = f.Name
	}
	return out, nil
}
