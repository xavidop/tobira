package dynamodb

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/xavidop/tobira/driver"
)

// maxStatementLimit caps the per-call Limit sent with a limited query.
const maxStatementLimit = 1000

// Query runs one PartiQL statement with ExecuteStatement, following
// NextToken page by page as rows are read. Query.Params bind the
// statement's ? placeholders in order. On a read-only call the statement
// must pass checkReadOnly first. With Query.Limit, each call of a SELECT
// asks for at most Limit + 1 items, and the result stops after Limit rows,
// reading one more to set Stats.Truncated.
//
// A SELECT's columns are its plain projected names, in order, or for
// SELECT * (or any expression) the table's key attributes first, then the
// other attributes of the items read ahead, sorted. When the FROM names a
// table, its key attributes are flagged and the rows can be edited.
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	const op = "query"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if err := checkNamespace(q.Namespace, op); err != nil {
		return nil, err
	}
	if q.ReadOnly || c.readOnly {
		if err := checkReadOnly(q.Text); err != nil {
			return nil, err
		}
	}
	start := time.Now()
	in := &dynamodb.ExecuteStatementInput{Statement: aws.String(q.Text)}
	for _, p := range q.Params {
		av, err := toAV(p)
		if err != nil {
			return nil, err
		}
		in.Parameters = append(in.Parameters, av)
	}
	toks, lexErr := lex(q.Text)
	isSelect := lexErr == nil && len(toks) > 0 && toks[0].isWord("SELECT")
	if q.Limit > 0 && isSelect {
		in.Limit = aws.Int32(statementLimit(q.Limit))
	}
	next := func(ctx context.Context) ([]item, bool, error) {
		out, err := c.db.ExecuteStatement(ctx, in)
		if err != nil {
			return nil, false, c.fail(ctx, op, err)
		}
		in.NextToken = out.NextToken
		return out.Items, out.NextToken != nil, nil
	}
	first, more, err := next(ctx)
	if err != nil {
		return nil, err
	}
	if !more {
		next = nil
	}
	var spec resultSpec
	if isSelect {
		sh := selectShape(toks)
		spec.projection = sh.columns
		if sh.table != "" {
			if meta, err := c.catalog.meta(ctx, op, sh.table); err == nil {
				spec.keys = meta.keys
				spec.source = &driver.CollectionRef{Namespace: namespaceName, Name: sh.table}
			}
		}
	}
	return c.newItemResult(ctx, op, next, first, q.Limit, start, spec)
}

// statementLimit is the per-call Limit for a query limited to limit rows:
// limit + 1 (to detect truncation), at most maxStatementLimit, computed
// without overflow.
func statementLimit(limit int) int32 {
	if limit >= maxStatementLimit {
		return maxStatementLimit
	}
	return int32(limit + 1)
}
