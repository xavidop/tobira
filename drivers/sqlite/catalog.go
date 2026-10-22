package sqlite

import (
	"context"
	"database/sql"
	"slices"

	"github.com/xavidop/tobira/driver"
)

func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	if err := c.check(ctx, "namespaces"); err != nil {
		return nil, err
	}
	rows, err := c.ro.QueryContext(ctx, "pragma database_list")
	if err != nil {
		return nil, mapError("namespaces", err)
	}
	defer func() { _ = rows.Close() }()
	var out []driver.Namespace
	for rows.Next() {
		var seq int
		var name string
		var file sql.NullString
		if err := rows.Scan(&seq, &name, &file); err != nil {
			return nil, mapError("namespaces", err)
		}
		out = append(out, driver.Namespace{Name: name, Label: "database", Default: name == "main"})
	}
	return out, mapError("namespaces", rows.Err())
}

func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	if err := c.check(ctx, "collections"); err != nil {
		return nil, err
	}
	name := nsName(ns.Name)
	rows, err := c.ro.QueryContext(ctx, "select name, type from "+quoteIdent(name)+
		`.sqlite_schema where type in ('table','view') and name not like 'sqlite\_%' escape '\' order by name`)
	if err != nil {
		return nil, mapError("collections", err)
	}
	defer func() { _ = rows.Close() }()
	var out []driver.Collection
	for rows.Next() {
		var n, typ string
		if err := rows.Scan(&n, &typ); err != nil {
			return nil, mapError("collections", err)
		}
		ct := driver.CollectionTable
		if typ == "view" {
			ct = driver.CollectionView
		}
		out = append(out, driver.Collection{Ref: driver.CollectionRef{Namespace: name, Name: n}, Type: ct, RowEstimate: -1})
	}
	return out, mapError("collections", rows.Err())
}

// queryer is the read half of *sql.DB and *sql.Conn.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	if err := c.check(ctx, "describe"); err != nil {
		return nil, err
	}
	return describeOn(ctx, c.ro, ref)
}

// describeOn describes ref over db: the read-only handle, or a transaction's
// own connection so it sees tables the transaction created.
func describeOn(ctx context.Context, db queryer, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	const op = "describe"
	ns := nsName(ref.Namespace)
	q := quoteIdent(ns)
	s := &driver.CollectionSchema{Ref: driver.CollectionRef{Namespace: ns, Name: ref.Name}, RowEstimate: -1, SizeBytes: -1}

	var typ string
	var ddl sql.NullString
	err := db.QueryRowContext(ctx, "select type, sql from "+q+".sqlite_schema where name = ? and type in ('table','view')", ref.Name).Scan(&typ, &ddl)
	if err == sql.ErrNoRows {
		return nil, driver.Wrap(driver.ErrNotFound, Name, op, errNoCollection(ns, ref.Name))
	}
	if err != nil {
		return nil, mapError(op, err)
	}
	s.Type, s.DDL = driver.CollectionTable, ddl.String
	if typ == "view" {
		s.Type = driver.CollectionView
	}

	tbl := quoteIdent(ref.Name)
	if err := describeFields(ctx, db, s, q, tbl); err != nil {
		return nil, mapError(op, err)
	}
	if err := describeIndexes(ctx, db, s, q, tbl); err != nil {
		return nil, mapError(op, err)
	}
	if err := describeFKs(ctx, db, s, q, tbl); err != nil {
		return nil, mapError(op, err)
	}
	return s, nil
}

func describeFields(ctx context.Context, db queryer, s *driver.CollectionSchema, q, tbl string) error {
	rows, err := db.QueryContext(ctx, "pragma "+q+".table_xinfo("+tbl+")")
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	type pkCol struct {
		pos  int
		name string
	}
	var pks []pkCol
	for rows.Next() {
		var cid, notnull, pk, hidden int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk, &hidden); err != nil {
			return err
		}
		if hidden == 1 {
			continue
		}
		s.Fields = append(s.Fields, driver.Field{
			Name: name, NativeType: typ, Type: mapType(typ), Nullable: notnull == 0,
			Default: dflt.String, Generated: hidden == 2 || hidden == 3,
		})
		if pk > 0 {
			pks = append(pks, pkCol{pk, name})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	slices.SortFunc(pks, func(a, b pkCol) int { return a.pos - b.pos })
	for _, p := range pks {
		s.PrimaryKey = append(s.PrimaryKey, p.name)
	}
	markRowidAlias(s)
	return nil
}

func describeIndexes(ctx context.Context, db queryer, s *driver.CollectionSchema, q, tbl string) error {
	rows, err := db.QueryContext(ctx, "pragma "+q+".index_list("+tbl+")")
	if err != nil {
		return err
	}
	var idx []driver.Index
	for rows.Next() {
		var seq, unique, partial int
		var name, origin string
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			_ = rows.Close()
			return err
		}
		idx = append(idx, driver.Index{Name: name, Unique: unique != 0, Primary: origin == "pk", Method: "btree"})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for i := range idx {
		ir, err := db.QueryContext(ctx, "pragma "+q+".index_info("+quoteIdent(idx[i].Name)+")")
		if err != nil {
			return err
		}
		for ir.Next() {
			var seqno, cid int
			var col sql.NullString
			if err := ir.Scan(&seqno, &cid, &col); err != nil {
				_ = ir.Close()
				return err
			}
			if col.Valid {
				idx[i].Fields = append(idx[i].Fields, col.String)
			}
		}
		err = ir.Err()
		_ = ir.Close()
		if err != nil {
			return err
		}
	}
	s.Indexes = idx
	return nil
}

func describeFKs(ctx context.Context, db queryer, s *driver.CollectionSchema, q, tbl string) error {
	rows, err := db.QueryContext(ctx, "pragma "+q+".foreign_key_list("+tbl+")")
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	byID := map[int]int{} // fk id -> index in s.ForeignKeys
	for rows.Next() {
		var id, seq int
		var table, from, onUpdate, onDelete, match string
		var to sql.NullString
		if err := rows.Scan(&id, &seq, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			return err
		}
		i, ok := byID[id]
		if !ok {
			i = len(s.ForeignKeys)
			byID[id] = i
			s.ForeignKeys = append(s.ForeignKeys, driver.ForeignKey{References: driver.CollectionRef{Namespace: s.Ref.Namespace, Name: table}})
		}
		fk := &s.ForeignKeys[i]
		fk.Fields = append(fk.Fields, from)
		if to.Valid {
			fk.RefFields = append(fk.RefFields, to.String)
		}
	}
	return rows.Err()
}
