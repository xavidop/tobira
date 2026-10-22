package snowflake

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

// snowflakeReserved is Snowflake's reserved keyword list
// (https://docs.snowflake.com/en/sql-reference/reserved-keywords).
const snowflakeReserved = `ACCOUNT ALL ALTER AND ANY AS BETWEEN BY CASE CAST CHECK COLUMN CONNECT CONNECTION CONSTRAINT
CREATE CROSS CURRENT CURRENT_DATE CURRENT_TIME CURRENT_TIMESTAMP CURRENT_USER DATABASE DELETE DISTINCT DROP ELSE EXISTS
FALSE FOLLOWING FOR FROM FULL GRANT GROUP GSCLUSTER HAVING ILIKE IN INCREMENT INNER INSERT INTERSECT INTO IS ISSUE JOIN
LATERAL LEFT LIKE LOCALTIME LOCALTIMESTAMP MINUS NATURAL NOT NULL OF ON OR ORDER ORGANIZATION QUALIFY REGEXP REVOKE
RIGHT RLIKE ROW ROWS SAMPLE SCHEMA SELECT SET SOME START TABLE TABLESAMPLE THEN TO TRIGGER TRUE TRY_CAST UNION UNIQUE
UPDATE USING VALUES VIEW WHEN WHENEVER WHERE WINDOW WITH`

const snowflakeKeywords = `SELECT FROM WHERE AND OR NOT NULL IS IN LIKE ILIKE RLIKE BETWEEN EXISTS AS ON USING JOIN INNER
LEFT RIGHT FULL OUTER CROSS NATURAL LATERAL GROUP BY ORDER HAVING QUALIFY LIMIT OFFSET FETCH UNION ALL INTERSECT
EXCEPT MINUS DISTINCT ASC DESC NULLS FIRST LAST INSERT INTO VALUES UPDATE SET DELETE MERGE CREATE ALTER DROP TABLE VIEW
SCHEMA DATABASE WAREHOUSE ROLE CASE WHEN THEN ELSE END CAST TRY_CAST WITH RECURSIVE BEGIN COMMIT ROLLBACK EXPLAIN
SHOW DESCRIBE COUNT SUM AVG MIN MAX COALESCE IFF NVL TRUE FALSE OVER PARTITION WINDOW SAMPLE TABLESAMPLE FLATTEN
PIVOT UNPIVOT MATCH_RECOGNIZE AT BEFORE CHANGES VARIANT OBJECT ARRAY TIMESTAMP_NTZ TIMESTAMP_TZ TIMESTAMP_LTZ`

// dialect is the Snowflake completion dialect. Unquoted names fold to
// upper case, so Complete re-quotes what sqlcomplete renders (see
// sfInsert).
var dialect = sqlcomplete.Dialect{
	Name:       "snowflake",
	Keywords:   strings.Fields(snowflakeKeywords),
	Reserved:   strings.Fields(snowflakeReserved),
	IdentQuote: '"',
}

// Complete suggests keywords, namespaces, tables and columns for the query
// text around the cursor. Catalog reads go through Namespaces, Collections
// and Describe and are cached for sqlcomplete.DefaultTTL; Invalidate drops
// the cache. A name that is not all upper case (or is reserved) is
// inserted in double quotes, since Snowflake folds unquoted names to upper
// case; a namespace is inserted as database.schema.
func (c *conn) Complete(ctx context.Context, req driver.CompletionRequest) ([]driver.Completion, error) {
	if err := c.check(ctx, "complete"); err != nil {
		return nil, err
	}
	out, err := sqlcomplete.Complete(ctx, c.catalog, dialect, req)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Insert = sfInsert(out[i])
	}
	return out, nil
}

// sfInsert returns the Insert text for a completion under Snowflake's
// quoting rules. A namespace is written as database.schema, each part as
// sqlName writes it; another name the user opened a quote for is already
// quoted.
func sfInsert(c driver.Completion) string {
	switch c.Kind {
	case "keyword", "function":
		return c.Insert
	case "namespace":
		db, schema, err := splitNamespace(c.Label)
		if err != nil {
			return c.Insert
		}
		s := sqlName(db)
		if schema != "" {
			s += "." + sqlName(schema)
		}
		return s + "."
	}
	if strings.HasPrefix(c.Insert, `"`) {
		return c.Insert
	}
	if s := sqlName(c.Label); s != c.Label {
		return s
	}
	return c.Insert
}

// sqlName writes a stored name as SQL: bare when Snowflake would read the
// bare word back as the same name (upper case letters, digits, '_' and '$',
// not starting with a digit or '$', not reserved), quoted otherwise.
func sqlName(name string) string {
	if name == "" {
		return quoteIdent(name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'A' && c <= 'Z', c == '_':
		case (c >= '0' && c <= '9') || c == '$':
			if i == 0 {
				return quoteIdent(name)
			}
		default:
			return quoteIdent(name)
		}
	}
	for _, r := range dialect.Reserved {
		if r == name {
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
