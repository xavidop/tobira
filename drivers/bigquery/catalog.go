package bigquery

import (
	"context"
	"errors"
	"fmt"
	"strings"

	bq "cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"

	"github.com/xavidop/tobira/driver"
)

// maxCatalogItems bounds the datasets and tables listed.
const maxCatalogItems = 10000

// Namespaces lists the project's datasets (hidden ones excluded), at most
// maxCatalogItems.
func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	if err := c.check(ctx, "namespaces"); err != nil {
		return nil, err
	}
	it := c.client.Datasets(ctx)
	var out []driver.Namespace
	for len(out) < maxCatalogItems {
		ds, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, c.fail(ctx, "namespaces", err)
		}
		out = append(out, driver.Namespace{Name: ds.DatasetID, Label: "dataset", Default: ds.DatasetID == c.dsn.dataset})
	}
	return out, nil
}

// Collections lists a dataset's tables, views, materialized views and the
// rest through the REST tables.list (the client library drops the table
// type), at most maxCatalogItems. The list carries no row counts.
func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	if err := c.check(ctx, "collections"); err != nil {
		return nil, err
	}
	ds := c.dataset(ns.Name)
	if ds == "" {
		return nil, driver.Wrap(driver.ErrNotFound, Name, "collections", errors.New("no dataset: name one, or put it in the DSN"))
	}
	var out []driver.Collection
	token := ""
	for len(out) < maxCatalogItems {
		call := c.svc.Tables.List(c.dsn.project, ds).MaxResults(1000).Context(ctx)
		if token != "" {
			call = call.PageToken(token)
		}
		page, err := call.Do()
		if err != nil {
			return nil, c.fail(ctx, "collections", err)
		}
		for _, t := range page.Tables {
			if t.TableReference == nil || len(out) >= maxCatalogItems {
				continue
			}
			out = append(out, driver.Collection{
				Ref:         driver.CollectionRef{Namespace: ds, Name: t.TableReference.TableId},
				Type:        collectionType(t.Type),
				RowEstimate: -1,
			})
		}
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}
	return out, nil
}

// collectionType maps a tables.list or metadata type.
func collectionType(t string) driver.CollectionType {
	switch strings.ToUpper(t) {
	case string(bq.RegularTable):
		return driver.CollectionTable
	case string(bq.ViewTable):
		return driver.CollectionView
	case string(bq.MaterializedView):
		return driver.CollectionMaterializedView
	}
	return driver.CollectionOther
}

// Describe reads a table's metadata: the schema (nested for STRUCT), the
// declared primary and foreign keys (BigQuery does not enforce them), the
// partitioning and clustering as indexes, row and byte counts, the
// description, and a view's query as its DDL.
func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	if err := c.check(ctx, "describe"); err != nil {
		return nil, err
	}
	ds := c.dataset(ref.Namespace)
	if ds == "" {
		return nil, driver.Wrap(driver.ErrNotFound, Name, "describe", errors.New("no dataset: name one, or put it in the DSN"))
	}
	md, err := c.client.Dataset(ds).Table(ref.Name).Metadata(ctx)
	if err != nil {
		return nil, c.fail(ctx, "describe", err)
	}
	s := &driver.CollectionSchema{
		Ref:         driver.CollectionRef{Namespace: ds, Name: ref.Name},
		Type:        collectionType(string(md.Type)),
		Fields:      fields(md.Schema),
		RowEstimate: int64(md.NumRows), //nolint:gosec // row counts fit in int64
		SizeBytes:   md.NumBytes,
		Comment:     md.Description,
	}
	if md.ViewQuery != "" {
		s.DDL = md.ViewQuery
	} else if md.MaterializedView != nil {
		s.DDL = md.MaterializedView.Query
	}
	if tc := md.TableConstraints; tc != nil {
		if tc.PrimaryKey != nil {
			s.PrimaryKey = append([]string(nil), tc.PrimaryKey.Columns...)
			s.Indexes = append(s.Indexes, driver.Index{Name: "PRIMARY KEY (NOT ENFORCED)", Fields: s.PrimaryKey, Primary: true, Unique: true})
		}
		for _, fk := range tc.ForeignKeys {
			if fk == nil || fk.ReferencedTable == nil {
				continue
			}
			f := driver.ForeignKey{Name: fk.Name, References: driver.CollectionRef{Namespace: fk.ReferencedTable.DatasetID, Name: fk.ReferencedTable.TableID}}
			for _, cr := range fk.ColumnReferences {
				f.Fields = append(f.Fields, cr.ReferencingColumn)
				f.RefFields = append(f.RefFields, cr.ReferencedColumn)
			}
			s.ForeignKeys = append(s.ForeignKeys, f)
		}
	}
	if p := md.TimePartitioning; p != nil {
		field := p.Field
		if field == "" {
			field = "_PARTITIONTIME"
		}
		s.Indexes = append(s.Indexes, driver.Index{Name: "partitioning", Fields: []string{field}, Method: fmt.Sprintf("time partitioning by %s", p.Type)})
	}
	if p := md.RangePartitioning; p != nil {
		s.Indexes = append(s.Indexes, driver.Index{Name: "partitioning", Fields: []string{p.Field}, Method: "integer range partitioning"})
	}
	if cl := md.Clustering; cl != nil && len(cl.Fields) > 0 {
		s.Indexes = append(s.Indexes, driver.Index{Name: "clustering", Fields: append([]string(nil), cl.Fields...), Method: "clustering"})
	}
	return s, nil
}

func fields(schema bq.Schema) []driver.Field {
	out := make([]driver.Field, len(schema))
	for i, fs := range schema {
		out[i] = driver.Field{
			Name:       fs.Name,
			NativeType: nativeType(fs),
			Type:       fieldType(fs),
			Nullable:   !fs.Required && !fs.Repeated,
			Default:    fs.DefaultValueExpression,
			Comment:    fs.Description,
		}
		if fs.Type == bq.RecordFieldType {
			out[i].Fields = fields(fs.Schema)
		}
	}
	return out
}
