package d1

import (
	"context"
	"errors"
	"fmt"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlitecore"
)

// A D1 database is one SQLite "main" schema (D1 refuses ATTACH), read with
// sqlite_master and the PRAGMAs D1 documents as supported: table_xinfo,
// index_list, index_info and foreign_key_list. D1's own tables (_cf_*)
// are hidden.

const mainNS = "main"

var errOnlyMain = errors.New("a D1 database has one namespace, main")

// namespace checks ref's namespace: "" or main.
func namespace(op, ns string) error {
	if sqlitecore.NamespaceName(ns) != mainNS {
		return driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("%w: no namespace %q", errOnlyMain, ns))
	}
	return nil
}

// Namespaces returns main, the only one.
func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	if err := c.check(ctx, "namespaces"); err != nil {
		return nil, err
	}
	return []driver.Namespace{{Name: mainNS, Label: "database", Default: true}}, nil
}

func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	const op = "collections"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if err := namespace(op, ns.Name); err != nil {
		return nil, err
	}
	r, err := c.client.exec(ctx, `SELECT name, type FROM sqlite_master WHERE type IN ('table', 'view')`+
		` AND name NOT LIKE 'sqlite\_%' ESCAPE '\' AND name NOT LIKE '\_cf\_%' ESCAPE '\' ORDER BY name`, nil)
	if err != nil {
		return nil, mapError(op, err)
	}
	return sqlitecore.Collections(mainNS, r.Rows), nil
}

func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	const op = "describe"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if err := namespace(op, ref.Namespace); err != nil {
		return nil, err
	}
	s := &driver.CollectionSchema{Ref: driver.CollectionRef{Namespace: mainNS, Name: ref.Name}, RowEstimate: -1, SizeBytes: -1}
	head, err := c.client.exec(ctx, "SELECT type, sql FROM sqlite_master WHERE name = ? AND type IN ('table', 'view')", []any{ref.Name})
	if err != nil {
		return nil, mapError(op, err)
	}
	if len(head.Rows) == 0 || len(head.Rows[0]) < 2 {
		return nil, driver.Wrap(driver.ErrNotFound, Name, op, errNoCollection(mainNS, ref.Name))
	}
	s.Type, s.DDL = driver.CollectionTable, sqlitecore.AsString(head.Rows[0][1])
	if sqlitecore.AsString(head.Rows[0][0]) == "view" {
		s.Type = driver.CollectionView
	}
	tbl := sqlitecore.QuoteIdent(ref.Name)
	fields, err := c.client.exec(ctx, "PRAGMA table_xinfo("+tbl+")", nil)
	if err != nil {
		return nil, mapError(op, err)
	}
	sqlitecore.ApplyTableXInfo(s, fields.Rows)
	list, err := c.client.exec(ctx, "PRAGMA index_list("+tbl+")", nil)
	if err != nil {
		return nil, mapError(op, err)
	}
	s.Indexes = sqlitecore.IndexList(list.Rows)
	for i := range s.Indexes {
		info, err := c.client.exec(ctx, "PRAGMA index_info("+sqlitecore.QuoteIdent(s.Indexes[i].Name)+")", nil)
		if err != nil {
			return nil, mapError(op, err)
		}
		s.Indexes[i].Fields = sqlitecore.IndexColumns(info.Rows)
	}
	fks, err := c.client.exec(ctx, "PRAGMA foreign_key_list("+tbl+")", nil)
	if err != nil {
		return nil, mapError(op, err)
	}
	sqlitecore.ApplyForeignKeys(s, fks.Rows)
	return s, nil
}
