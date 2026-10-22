package oracle

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

// oracleReserved is V$RESERVED_WORDS with RESERVED = 'Y': words that must be
// quoted when used as an identifier.
const oracleReserved = `ACCESS ADD ALL ALTER AND ANY AS ASC AUDIT BETWEEN BY CHAR CHECK CLUSTER COLUMN COMMENT
COMPRESS CONNECT CREATE CURRENT DATE DECIMAL DEFAULT DELETE DESC DISTINCT DROP ELSE EXCLUSIVE EXISTS FILE FLOAT FOR
FROM GRANT GROUP HAVING IDENTIFIED IMMEDIATE IN INCREMENT INDEX INITIAL INSERT INTEGER INTERSECT INTO IS LEVEL LIKE
LOCK LONG MAXEXTENTS MINUS MLSLABEL MODE MODIFY NOAUDIT NOCOMPRESS NOT NOWAIT NULL NUMBER OF OFFLINE ON ONLINE
OPTION OR ORDER PCTFREE PRIOR PUBLIC RAW RENAME RESOURCE REVOKE ROW ROWID ROWNUM ROWS SELECT SESSION SET SHARE SIZE
SMALLINT START SUCCESSFUL SYNONYM SYSDATE TABLE THEN TO TRIGGER UID UNION UNIQUE UPDATE USER VALIDATE VALUES VARCHAR
VARCHAR2 VIEW WHENEVER WHERE WITH`

const oracleKeywords = `SELECT FROM WHERE AND OR NOT NULL IS IN LIKE BETWEEN EXISTS AS ON USING JOIN INNER LEFT
RIGHT FULL OUTER CROSS NATURAL GROUP BY ORDER HAVING UNION ALL INTERSECT MINUS DISTINCT ASC DESC INSERT INTO VALUES
UPDATE SET DELETE MERGE MATCHED CREATE ALTER DROP TABLE VIEW INDEX UNIQUE PRIMARY KEY FOREIGN REFERENCES DEFAULT
CHECK CONSTRAINT CASE WHEN THEN ELSE END CAST WITH BEGIN COMMIT ROLLBACK SAVEPOINT EXPLAIN PLAN COUNT SUM AVG MIN
MAX COALESCE NVL NVL2 DECODE TRUE FALSE RETURNING FETCH FIRST NEXT ROWS ONLY OFFSET NULLS LAST CONNECT PRIOR START
LEVEL ROWNUM ROWID SYSDATE SYSTIMESTAMP DUAL PIVOT UNPIVOT OVER PARTITION TO_CHAR TO_DATE TO_NUMBER TO_TIMESTAMP
SUBSTR INSTR LENGTH TRIM UPPER LOWER LISTAGG JSON_VALUE JSON_QUERY JSON_TABLE VARCHAR2 NUMBER DATE TIMESTAMP CLOB
BLOB BOOLEAN JSON INTERVAL TRUNCATE GRANT REVOKE SEQUENCE SYNONYM COMMENT`

// oracleDialect is sqlcomplete's view of Oracle SQL: unquoted identifiers
// fold to upper case, so Complete quotes names with lower-case letters
// itself (see quoteFolded).
func oracleDialect() sqlcomplete.Dialect {
	return sqlcomplete.Dialect{
		Name:       "oracle",
		Keywords:   strings.Fields(oracleKeywords),
		Reserved:   strings.Fields(oracleReserved),
		IdentQuote: '"',
	}
}

// Complete suggests keywords, schemas, tables and columns for the query
// text around the cursor. Catalog reads go through Namespaces, Collections
// and Describe and are cached for sqlcomplete.DefaultTTL; Invalidate drops
// the cache.
func (c *conn) Complete(ctx context.Context, req driver.CompletionRequest) ([]driver.Completion, error) {
	if err := c.check(ctx, "complete"); err != nil {
		return nil, err
	}
	out, err := sqlcomplete.Complete(ctx, c.catalog, oracleDialect(), req)
	if err != nil {
		return nil, err
	}
	for i := range out {
		quoteFolded(&out[i])
	}
	return out, nil
}

// quoteFolded quotes a schema, table or column name holding a lower-case
// letter, which Oracle would otherwise fold to upper case, unless the
// completion is quoted already.
func quoteFolded(c *driver.Completion) {
	switch c.Kind {
	case "namespace", "collection", "field":
	default:
		return
	}
	if strings.HasPrefix(c.Insert, `"`) || strings.ToUpper(c.Label) == c.Label {
		return
	}
	text := `"` + c.Label + `"`
	if c.Kind == "namespace" {
		text += "."
	}
	c.Insert = text
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
