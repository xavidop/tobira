package vertica

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

// verticaReserved is Vertica's reserved word list (SQL Reference, Keywords:
// reserved words).
const verticaReserved = `ALL ANALYSE ANALYZE AND ANY ARRAY AS ASC BINARY BOTH CASE CAST CHECK COLUMN CONSTRAINT CORRELATION
CREATE CURRENT_DATABASE CURRENT_DATE CURRENT_SCHEMA CURRENT_TIME CURRENT_TIMESTAMP CURRENT_USER DEFAULT DEFERRABLE DESC
DISTINCT DO ELSE ENCODED END EXCEPT FALSE FLEX FLEXIBLE FOR FOREIGN FROM GRANT GROUP GROUPED HAVING IN INITIALLY
INTERSECT INTERVAL INTERVALYM INTO JOIN KSAFE LEADING LIMIT LOCALTIME LOCALTIMESTAMP MATCH MINUS NEW NOT NULL NULLSEQUAL
OFFSET OFF OLD ON ONLY OR ORDER PINNED PLACING PRIMARY PROJECTION REFERENCES SCHEMA SEGMENTED SELECT SESSION_USER SOME
SYSDATE TABLE THEN TIMESERIES TO TRAILING TRUE UNBOUNDED UNION UNIQUE UNSEGMENTED USER USING WHEN WHERE WINDOW WITH WITHIN`

const verticaKeywords = `SELECT FROM WHERE AND OR NOT NULL IS IN LIKE ILIKE BETWEEN EXISTS AS ON USING JOIN INNER LEFT
RIGHT FULL OUTER CROSS NATURAL GROUP BY ORDER HAVING LIMIT OFFSET UNION ALL INTERSECT EXCEPT MINUS DISTINCT ASC DESC
INSERT INTO VALUES UPDATE SET DELETE MERGE CREATE ALTER DROP TABLE VIEW SCHEMA PROJECTION CASE WHEN THEN ELSE END
CAST WITH RECURSIVE BEGIN COMMIT ROLLBACK EXPLAIN PROFILE SHOW COUNT SUM AVG MIN MAX COALESCE NVL DECODE TRUE FALSE
OVER PARTITION TIMESERIES INTERPOLATE MATCH EPOCH AT LATEST COPY EXPORT TRUNCATE SEGMENTED UNSEGMENTED KSAFE
VARCHAR CHAR NUMERIC INT FLOAT BOOLEAN DATE TIMESTAMP TIMESTAMPTZ TIME INTERVAL UUID VARBINARY LONG ARRAY ROW`

// dialect is the Vertica completion dialect.
var dialect = sqlcomplete.Dialect{
	Name:       "vertica",
	Keywords:   strings.Fields(verticaKeywords),
	Reserved:   strings.Fields(verticaReserved),
	IdentQuote: '"',
}

// Complete suggests keywords, namespaces, tables and columns for the query
// text around the cursor. Catalog reads go through Namespaces, Collections
// and Describe and are cached for sqlcomplete.DefaultTTL; Invalidate drops
// the cache. Vertica names are case-insensitive, so a name is inserted bare
// unless it is not a plain identifier or is reserved.
func (c *conn) Complete(ctx context.Context, req driver.CompletionRequest) ([]driver.Completion, error) {
	if err := c.check(ctx, "complete"); err != nil {
		return nil, err
	}
	out, err := sqlcomplete.Complete(ctx, c.catalog, dialect, req)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Insert = verticaInsert(out[i])
	}
	return out, nil
}

// verticaInsert returns the Insert text for a completion. A namespace is
// followed by '.'; a name the user opened a quote for is already quoted.
func verticaInsert(c driver.Completion) string {
	switch c.Kind {
	case "keyword", "function":
		return c.Insert
	case "namespace":
		return sqlName(c.Label) + "."
	}
	if strings.HasPrefix(c.Insert, `"`) {
		return c.Insert
	}
	return sqlName(c.Label)
}

// sqlName writes a stored name as SQL: bare when it is a plain identifier
// (an ASCII letter or '_', then letters, digits, '_' and '$') that is not
// reserved, quoted otherwise.
func sqlName(name string) string {
	if name == "" || !isWordStart(name[0]) {
		return quoteIdent(name)
	}
	for i := 1; i < len(name); i++ {
		if !isWordByte(name[i]) {
			return quoteIdent(name)
		}
	}
	upper := strings.ToUpper(name)
	for _, r := range dialect.Reserved {
		if r == upper {
			return quoteIdent(name)
		}
	}
	return name
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
