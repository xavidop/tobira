package bigquery

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

// googleSQLReserved is GoogleSQL's reserved keyword list
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/lexical#reserved_keywords);
// such names must be quoted with backticks.
const googleSQLReserved = `ALL AND ANY ARRAY AS ASC ASSERT_ROWS_MODIFIED AT BETWEEN BY CASE CAST COLLATE CONTAINS
CREATE CROSS CUBE CURRENT DEFAULT DEFINE DESC DISTINCT ELSE END ENUM ESCAPE EXCEPT EXCLUDE EXISTS EXTRACT FALSE
FETCH FOLLOWING FOR FROM FULL GROUP GROUPING GROUPS HASH HAVING IF IGNORE IN INNER INTERSECT INTERVAL INTO IS JOIN
LATERAL LEFT LIKE LIMIT LOOKUP MERGE NATURAL NEW NO NOT NULL NULLS OF ON OR ORDER OUTER OVER PARTITION PRECEDING
PROTO QUALIFY RANGE RECURSIVE RESPECT RIGHT ROLLUP ROWS SELECT SET SOME STRUCT TABLESAMPLE THEN TO TREAT TRUE
UNBOUNDED UNION UNNEST USING WHEN WHERE WINDOW WITH WITHIN`

// googleSQLKeywords are offered in keyword contexts: the reserved words
// plus common clause words, types and functions.
const googleSQLKeywords = googleSQLReserved + ` OFFSET INSERT UPDATE DELETE VALUES TABLE VIEW
MATERIALIZED TRUNCATE DROP ALTER QUALIFY PIVOT UNPIVOT SYSTEM_TIME OPTIONS CLUSTER PARTITION REPLACE
INT64 FLOAT64 NUMERIC BIGNUMERIC BOOL STRING BYTES DATE DATETIME TIME TIMESTAMP JSON GEOGRAPHY
COUNT SUM AVG MIN MAX COALESCE IFNULL SAFE_CAST ARRAY_AGG STRING_AGG ARRAY_LENGTH GENERATE_ARRAY
CURRENT_DATE CURRENT_TIMESTAMP DATE_TRUNC TIMESTAMP_TRUNC FORMAT_TIMESTAMP PARSE_DATE JSON_VALUE
JSON_QUERY TO_JSON_STRING APPROX_COUNT_DISTINCT ROW_NUMBER RANK`

// dialect is GoogleSQL for completion: "#" comments, double-quoted
// strings and backslash escapes (sqlcomplete's MySQL lexical rules, which
// GoogleSQL shares), backtick quoting, and no case folding.
var dialect = sqlcomplete.Dialect{
	Name:        "googlesql",
	Keywords:    strings.Fields(googleSQLKeywords),
	Reserved:    strings.Fields(googleSQLReserved),
	MySQLSyntax: true,
	Backticks:   true,
	IdentQuote:  '`',
}

// Complete suggests keywords, datasets, tables and columns for the query
// text around the cursor. Catalog reads go through Namespaces, Collections
// and Describe (metadata calls, never billed) and are cached for
// sqlcomplete.DefaultTTL; Invalidate drops the cache. Names that need
// quoting are inserted with backticks.
func (c *conn) Complete(ctx context.Context, req driver.CompletionRequest) ([]driver.Completion, error) {
	if err := c.check(ctx, "complete"); err != nil {
		return nil, err
	}
	return sqlcomplete.Complete(ctx, c.catalog, dialect, req)
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

// Collections lists a dataset's tables; with no dataset named and none in
// the DSN there is nothing to offer.
func (r catalogReader) Collections(ctx context.Context, ns string) ([]string, error) {
	if r.c.dataset(ns) == "" {
		return nil, nil
	}
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
	if r.c.dataset(ns) == "" {
		return nil, nil
	}
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
