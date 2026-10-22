package mssql

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

// tsqlKeywords are offered in keyword contexts.
const tsqlKeywords = `SELECT FROM WHERE AND OR NOT NULL IS IN LIKE BETWEEN EXISTS AS ON
JOIN INNER LEFT RIGHT FULL OUTER CROSS APPLY GROUP BY ORDER HAVING OFFSET FETCH NEXT ROWS ONLY TOP PERCENT TIES
UNION ALL INTERSECT EXCEPT DISTINCT ASC DESC INSERT INTO VALUES UPDATE SET DELETE MERGE USING MATCHED OUTPUT INSERTED DELETED
CREATE ALTER DROP TABLE VIEW INDEX UNIQUE PRIMARY KEY FOREIGN REFERENCES DEFAULT CHECK CONSTRAINT IDENTITY
CASE WHEN THEN ELSE END CAST CONVERT TRY_CAST TRY_CONVERT ISNULL COALESCE NULLIF IIF WITH NOLOCK
BEGIN COMMIT ROLLBACK TRANSACTION TRAN SAVE DECLARE EXEC EXECUTE PROCEDURE FUNCTION RETURNS RETURN
COUNT SUM AVG MIN MAX OVER PARTITION ROW_NUMBER RANK DENSE_RANK GETDATE SYSUTCDATETIME DATEADD DATEDIFF
TRUNCATE SCHEMA DATABASE USE GO PIVOT UNPIVOT FOR XML JSON PATH OPTION RECOMPILE`

// tsqlReserved are SQL Server's reserved keywords, which must be quoted
// ([name]) when used as an identifier.
const tsqlReserved = `ADD ALL ALTER AND ANY AS ASC AUTHORIZATION BACKUP BEGIN BETWEEN BREAK BROWSE BULK BY CASCADE CASE
CHECK CHECKPOINT CLOSE CLUSTERED COALESCE COLLATE COLUMN COMMIT COMPUTE CONSTRAINT CONTAINS CONTAINSTABLE CONTINUE
CONVERT CREATE CROSS CURRENT CURRENT_DATE CURRENT_TIME CURRENT_TIMESTAMP CURRENT_USER CURSOR DATABASE DBCC
DEALLOCATE DECLARE DEFAULT DELETE DENY DESC DISK DISTINCT DISTRIBUTED DOUBLE DROP DUMP ELSE END ERRLVL ESCAPE
EXCEPT EXEC EXECUTE EXISTS EXIT EXTERNAL FETCH FILE FILLFACTOR FOR FOREIGN FREETEXT FREETEXTTABLE FROM FULL
FUNCTION GOTO GRANT GROUP HAVING HOLDLOCK IDENTITY IDENTITY_INSERT IDENTITYCOL IF IN INDEX INNER INSERT INTERSECT
INTO IS JOIN KEY KILL LEFT LIKE LINENO LOAD MERGE NATIONAL NOCHECK NONCLUSTERED NOT NULL NULLIF OF OFF OFFSETS
ON OPEN OPENDATASOURCE OPENQUERY OPENROWSET OPENXML OPTION OR ORDER OUTER OVER PERCENT PIVOT PLAN PRECISION
PRIMARY PRINT PROC PROCEDURE PUBLIC RAISERROR READ READTEXT RECONFIGURE REFERENCES REPLICATION RESTORE RESTRICT
RETURN REVERT REVOKE RIGHT ROLLBACK ROWCOUNT ROWGUIDCOL RULE SAVE SCHEMA SECURITYAUDIT SELECT
SEMANTICKEYPHRASETABLE SEMANTICSIMILARITYDETAILSTABLE SEMANTICSIMILARITYTABLE SESSION_USER SET SETUSER SHUTDOWN
SOME STATISTICS SYSTEM_USER TABLE TABLESAMPLE TEXTSIZE THEN TO TOP TRAN TRANSACTION TRIGGER TRUNCATE TRY_CONVERT
TSEQUAL UNION UNIQUE UNPIVOT UPDATE UPDATETEXT USE USER VALUES VARYING VIEW WAITFOR WHEN WHERE WHILE WITH
WITHIN WRITETEXT`

// tsqlDialect is the completion dialect: T-SQL keywords, names quoted with
// brackets. It is built here from sqlcomplete's exported fields, so the
// shared package needs no T-SQL entry.
func tsqlDialect() sqlcomplete.Dialect {
	return sqlcomplete.Dialect{
		Name:       "tsql",
		Keywords:   strings.Fields(tsqlKeywords),
		Reserved:   strings.Fields(tsqlReserved),
		Brackets:   true,
		IdentQuote: '[',
	}
}

// Complete suggests keywords, schemas, tables and columns for the query text
// around the cursor. Catalog reads go through Namespaces, Collections and
// Describe and are cached for sqlcomplete.DefaultTTL; Invalidate drops the
// cache. Names that need quoting are inserted with brackets.
func (c *conn) Complete(ctx context.Context, req driver.CompletionRequest) ([]driver.Completion, error) {
	if err := c.check(ctx, "complete"); err != nil {
		return nil, err
	}
	return sqlcomplete.Complete(ctx, c.catalog, tsqlDialect(), req)
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
