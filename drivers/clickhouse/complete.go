package clickhouse

import (
	"context"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

var (
	_ driver.Completer          = (*conn)(nil)
	_ driver.CatalogInvalidator = (*conn)(nil)
)

// Complete suggests keywords, databases, tables and columns for the query
// text around the cursor. Catalog reads go through Namespaces, Collections
// and Describe, which read the system tables with readonly=1, and are cached
// for sqlcomplete.DefaultTTL; Invalidate drops the cache. Names that need
// quoting are inserted with backticks.
func (c *conn) Complete(ctx context.Context, req driver.CompletionRequest) ([]driver.Completion, error) {
	if err := c.check(ctx, "complete"); err != nil {
		return nil, err
	}
	return sqlcomplete.Complete(ctx, c.catalog, sqlcomplete.ClickHouse(), req)
}

// Invalidate drops the cached catalog so the next completion sees new tables.
func (c *conn) Invalidate() { c.catalog.Invalidate() }

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
