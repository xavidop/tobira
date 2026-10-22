package duckdb

import (
	"context"
	"strings"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

var (
	_ driver.Completer          = (*conn)(nil)
	_ driver.CatalogInvalidator = (*conn)(nil)
)

// completeDialect is DuckDB for completion: PostgreSQL's lexical rules and
// reserved words, DuckDB's own statement words, and case-insensitive names
// (DuckDB keeps a name's case but matches it in any case).
var completeDialect = func() sqlcomplete.Dialect {
	d := sqlcomplete.Postgres()
	d.Name = "duckdb"
	d.LowerFold = false
	d.Keywords = append(d.Keywords, strings.Fields(`DESCRIBE SHOW SUMMARIZE PIVOT UNPIVOT QUALIFY EXCLUDE
REPLACE COLUMNS STRUCT MAP LIST UNION HUGEINT UUID BLOB INTERVAL ATTACH DETACH PRAGMA INSTALL LOAD EXPORT IMPORT
MACRO SEQUENCE TYPE ENUM SAMPLE ASOF POSITIONAL ANTI SEMI`)...)
	return d
}()

// Complete suggests keywords, schemas, tables and columns for the text
// around the cursor, reading the catalog through a cache that expires after
// sqlcomplete.DefaultTTL; Invalidate drops it.
func (c *conn) Complete(ctx context.Context, req driver.CompletionRequest) ([]driver.Completion, error) {
	if err := c.check(ctx, "complete"); err != nil {
		return nil, err
	}
	return sqlcomplete.Complete(ctx, c.catalog, completeDialect, req)
}

// Invalidate drops the cached catalog so the next completion sees new
// tables.
func (c *conn) Invalidate() { c.catalog.Invalidate() }

// catalogReader adapts the connection to sqlcomplete.Catalog.
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
