package cassandra

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// systemKeyspace reports whether a keyspace is Cassandra's own (system,
// system_auth, system_schema, system_views …), which Namespaces leaves
// out unless the DSN sets system_keyspaces=true.
func systemKeyspace(name string) bool { return name == "system" || strings.HasPrefix(name, "system_") }

// Namespaces lists the keyspaces from system_schema.keyspaces, without the
// system ones unless the DSN asks for them. The DSN's keyspace is the
// default.
func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	const op = "namespaces"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	iter := c.session.Query("SELECT keyspace_name FROM system_schema.keyspaces").Idempotent(true).IterContext(ctx)
	var (
		name string
		out  []driver.Namespace
	)
	for iter.Scan(&name) {
		if systemKeyspace(name) && !c.set.systemKeyspaces {
			continue
		}
		out = append(out, driver.Namespace{Name: name, Label: "keyspace", Default: name == c.set.keyspace})
	}
	if err := iter.Close(); err != nil {
		return nil, mapError(ctx, op, err)
	}
	slices.SortFunc(out, func(a, b driver.Namespace) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Collections lists a keyspace's tables (system_schema.tables) and
// materialized views (system_schema.views), with their comments. Row
// counts are unknown (-1): Cassandra only estimates them per node.
func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	const op = "collections"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	ks := c.keyspace(ns.Name)
	if ks == "" {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("no keyspace: name one or set it in the DSN"))
	}
	var out []driver.Collection
	for _, src := range []struct {
		table string
		typ   driver.CollectionType
	}{{"tables", driver.CollectionTable}, {"views", driver.CollectionMaterializedView}} {
		iter := c.session.Query("SELECT "+src.table[:len(src.table)-1]+"_name, comment FROM system_schema."+src.table+" WHERE keyspace_name = ?", ks).
			Idempotent(true).IterContext(ctx)
		var name, comment string
		for iter.Scan(&name, &comment) {
			out = append(out, driver.Collection{
				Ref: driver.CollectionRef{Namespace: ks, Name: name}, Type: src.typ, RowEstimate: -1, Comment: comment,
			})
		}
		if err := iter.Close(); err != nil {
			return nil, mapError(ctx, op, err)
		}
	}
	slices.SortFunc(out, func(a, b driver.Collection) int { return strings.Compare(a.Ref.Name, b.Ref.Name) })
	return out, nil
}

// column is one row of system_schema.columns.
type column struct {
	name, kind, order, typ string
	position               int
}

// Describe reads a table's or view's columns from system_schema.columns:
// the partition key columns, then the clustering columns (each with its
// clustering order), then the static and regular columns by name, as
// Cassandra lists them. PartitionKey, ClusteringKey and PrimaryKey come
// from the column kinds; key columns are not nullable. Secondary indexes
// come from system_schema.indexes. A table that does not exist is
// ErrNotFound. Answers are cached for schemaTTL (see schemaCache).
func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	const op = "describe"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	ks := c.keyspace(ref.Namespace)
	if ks == "" {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("no keyspace: name one or set it in the DSN"))
	}
	ref = driver.CollectionRef{Namespace: ks, Name: ref.Name}
	return c.schemas.get(ctx, ref, func(ctx context.Context) (*driver.CollectionSchema, error) { return c.describe(ctx, ref) })
}

// describe reads ref's schema from system_schema (see Describe).
func (c *conn) describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	const op = "describe"
	ks := ref.Namespace
	sch := &driver.CollectionSchema{Ref: ref, RowEstimate: -1, SizeBytes: -1}
	typ, comment, err := c.tableType(ctx, ks, ref.Name)
	if err != nil {
		return nil, err
	}
	sch.Type, sch.Comment = typ, comment
	cols, err := c.columns(ctx, ks, ref.Name)
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("table %s.%s has no columns", ks, ref.Name))
	}
	for _, col := range cols {
		key := col.kind == "partition_key" || col.kind == "clustering"
		sch.Fields = append(sch.Fields, driver.Field{
			Name: col.name, NativeType: col.typ, Type: typeOfNative(col.typ), Nullable: !key,
		})
		switch col.kind {
		case "partition_key":
			sch.PartitionKey = append(sch.PartitionKey, col.name)
		case "clustering":
			sch.ClusteringKey = append(sch.ClusteringKey, driver.SortKey{Field: col.name, Desc: col.order == "desc"})
		}
	}
	sch.PrimaryKey = slices.Clone(sch.PartitionKey)
	for _, k := range sch.ClusteringKey {
		sch.PrimaryKey = append(sch.PrimaryKey, k.Field)
	}
	sch.Indexes = append(sch.Indexes, driver.Index{Name: "PRIMARY", Fields: sch.PrimaryKey, Unique: true, Primary: true})
	idx, err := c.indexes(ctx, ks, ref.Name)
	if err != nil {
		return nil, err
	}
	sch.Indexes = append(sch.Indexes, idx...)
	return sch, nil
}

// tableType finds ref in system_schema.tables, then views; neither is
// ErrNotFound.
func (c *conn) tableType(ctx context.Context, ks, name string) (driver.CollectionType, string, error) {
	var comment string
	err := c.session.Query("SELECT comment FROM system_schema.tables WHERE keyspace_name = ? AND table_name = ?", ks, name).
		Idempotent(true).ScanContext(ctx, &comment)
	if err == nil {
		return driver.CollectionTable, comment, nil
	}
	if !errors.Is(mapError(ctx, "describe", err), driver.ErrNotFound) {
		return "", "", mapError(ctx, "describe", err)
	}
	err = c.session.Query("SELECT comment FROM system_schema.views WHERE keyspace_name = ? AND view_name = ?", ks, name).
		Idempotent(true).ScanContext(ctx, &comment)
	if err == nil {
		return driver.CollectionMaterializedView, comment, nil
	}
	err = mapError(ctx, "describe", err)
	if errors.Is(err, driver.ErrNotFound) {
		return "", "", driver.Wrap(driver.ErrNotFound, Name, "describe", fmt.Errorf("table %s.%s does not exist", ks, name))
	}
	return "", "", err
}

// columnKinds orders a table's columns as Cassandra lists them.
var columnKinds = map[string]int{"partition_key": 0, "clustering": 1, "static": 2, "regular": 3}

func (c *conn) columns(ctx context.Context, ks, table string) ([]column, error) {
	iter := c.session.Query("SELECT column_name, kind, position, clustering_order, type FROM system_schema.columns "+
		"WHERE keyspace_name = ? AND table_name = ?", ks, table).Idempotent(true).IterContext(ctx)
	var (
		col  column
		cols []column
	)
	for iter.Scan(&col.name, &col.kind, &col.position, &col.order, &col.typ) {
		cols = append(cols, col)
	}
	if err := iter.Close(); err != nil {
		return nil, mapError(ctx, "describe", err)
	}
	slices.SortFunc(cols, func(a, b column) int {
		if d := columnKinds[a.kind] - columnKinds[b.kind]; d != 0 {
			return d
		}
		if a.kind == "partition_key" || a.kind == "clustering" {
			return a.position - b.position
		}
		return strings.Compare(a.name, b.name)
	})
	return cols, nil
}

// indexes reads a table's secondary indexes: their target column and their
// class (custom indexes such as SAI) as the Method.
func (c *conn) indexes(ctx context.Context, ks, table string) ([]driver.Index, error) {
	iter := c.session.Query("SELECT index_name, kind, options FROM system_schema.indexes WHERE keyspace_name = ? AND table_name = ?",
		ks, table).Idempotent(true).IterContext(ctx)
	var (
		name, kind string
		opts       map[string]string
		out        []driver.Index
	)
	for iter.Scan(&name, &kind, &opts) {
		ix := driver.Index{Name: name, Method: strings.ToLower(kind)}
		if cls := opts["class_name"]; cls != "" {
			ix.Method = cls
		}
		if target := opts["target"]; target != "" {
			ix.Fields = []string{target}
		}
		out = append(out, ix)
		opts = nil
	}
	if err := iter.Close(); err != nil {
		return nil, mapError(ctx, "describe", err)
	}
	return out, nil
}
