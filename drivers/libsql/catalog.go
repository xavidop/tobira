package libsql

import (
	"context"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlitecore"
)

// The catalog mirrors the sqlite driver's (drivers/sqlite catalog.go):
// database_list, sqlite_schema and the table_xinfo, index_list, index_info
// and foreign_key_list pragmas, read as table-valued functions.

// execer runs statements and returns one result per statement: the
// connection's read-only path, or a transaction's stream.
type execer interface {
	execAll(ctx context.Context, stmts ...hranaStmt) ([]*stmtResult, error)
}

// roExecer is the connection's read-only path as an execer.
type roExecer struct{ c *conn }

func (r roExecer) execAll(ctx context.Context, stmts ...hranaStmt) ([]*stmtResult, error) {
	return r.c.readOnlyExecAll(ctx, stmts...)
}

func catalogStmt(sql string, args ...any) hranaStmt {
	s := hranaStmt{SQL: sql, WantRows: true}
	for _, a := range args {
		v, _ := encodeArg(a) // only strings are passed
		s.Args = append(s.Args, v)
	}
	return s
}

// rowsOf decodes r's rows; a nil result has none.
func rowsOf(r *stmtResult) ([][]any, error) {
	if r == nil {
		return nil, nil
	}
	return decodeRows(r)
}

func asString(v any) string { return sqlitecore.AsString(v) }

func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	const op = "namespaces"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	r, err := c.readOnlyExec(ctx, catalogStmt("select seq, name, file from pragma_database_list"))
	if err != nil {
		return nil, mapError(op, err)
	}
	rows, err := rowsOf(r)
	if err != nil {
		return nil, mapError(op, err)
	}
	var out []driver.Namespace
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		name := asString(row[1])
		out = append(out, driver.Namespace{Name: name, Label: "database", Default: name == "main"})
	}
	return out, nil
}

func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	const op = "collections"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	name := nsName(ns.Name)
	r, err := c.readOnlyExec(ctx, catalogStmt("select name, type from "+quoteIdent(name)+
		`.sqlite_schema where type in ('table','view') and name not like 'sqlite\_%' escape '\' order by name`))
	if err != nil {
		return nil, mapError(op, err)
	}
	rows, err := rowsOf(r)
	if err != nil {
		return nil, mapError(op, err)
	}
	return sqlitecore.Collections(name, rows), nil
}

func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	if err := c.check(ctx, "describe"); err != nil {
		return nil, err
	}
	return describeOn(ctx, roExecer{c}, ref)
}

// describeOn describes ref over ex in two round trips: the schema row and
// the column, index and foreign-key lists, then each index's columns. It
// reads the pragmas through their table-valued functions (SELECT … FROM
// pragma_table_xinfo(…)): sqld treats a bare PRAGMA as a write, which a
// read-only token refuses, but a SELECT as a read.
func describeOn(ctx context.Context, ex execer, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	const op = "describe"
	ns := nsName(ref.Namespace)
	q := quoteIdent(ns)
	s := &driver.CollectionSchema{Ref: driver.CollectionRef{Namespace: ns, Name: ref.Name}, RowEstimate: -1, SizeBytes: -1}

	res, err := ex.execAll(ctx,
		catalogStmt("select type, sql from "+q+".sqlite_schema where name = ? and type in ('table','view')", ref.Name),
		catalogStmt(`select cid, name, type, "notnull", dflt_value, pk, hidden from pragma_table_xinfo(?, ?)`, ref.Name, ns),
		catalogStmt(`select seq, name, "unique", origin, partial from pragma_index_list(?, ?)`, ref.Name, ns),
		catalogStmt(`select id, seq, "table", "from", "to" from pragma_foreign_key_list(?, ?)`, ref.Name, ns),
	)
	if err != nil {
		return nil, mapError(op, err)
	}
	head, err := rowsOf(res[0])
	if err != nil {
		return nil, mapError(op, err)
	}
	if len(head) == 0 {
		return nil, driver.Wrap(driver.ErrNotFound, Name, op, errNoCollection(ns, ref.Name))
	}
	s.Type, s.DDL = driver.CollectionTable, asString(head[0][1])
	if asString(head[0][0]) == "view" {
		s.Type = driver.CollectionView
	}
	if err := describeFields(s, res[1]); err != nil {
		return nil, mapError(op, err)
	}
	if err := describeIndexes(ctx, ex, s, ns, res[2]); err != nil {
		return nil, mapError(op, err)
	}
	if err := describeFKs(s, res[3]); err != nil {
		return nil, mapError(op, err)
	}
	return s, nil
}

func describeFields(s *driver.CollectionSchema, r *stmtResult) error {
	rows, err := rowsOf(r)
	if err != nil {
		return err
	}
	sqlitecore.ApplyTableXInfo(s, rows)
	return nil
}

func describeIndexes(ctx context.Context, ex execer, s *driver.CollectionSchema, ns string, r *stmtResult) error {
	rows, err := rowsOf(r)
	if err != nil {
		return err
	}
	idx := sqlitecore.IndexList(rows)
	if len(idx) > 0 {
		stmts := make([]hranaStmt, len(idx))
		for i := range idx {
			stmts[i] = catalogStmt("select seqno, cid, name from pragma_index_info(?, ?)", idx[i].Name, ns)
		}
		res, err := ex.execAll(ctx, stmts...)
		if err != nil {
			return err
		}
		for i := range idx {
			cols, err := rowsOf(res[i])
			if err != nil {
				return err
			}
			idx[i].Fields = sqlitecore.IndexColumns(cols)
		}
	}
	s.Indexes = idx
	return nil
}

func describeFKs(s *driver.CollectionSchema, r *stmtResult) error {
	rows, err := rowsOf(r)
	if err != nil {
		return err
	}
	sqlitecore.ApplyForeignKeys(s, rows)
	return nil
}
