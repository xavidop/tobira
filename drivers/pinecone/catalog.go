package pinecone

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/xavidop/tobira/driver"
)

const (
	// maxIndexes and maxCollections cap the listings.
	maxIndexes     = 10000
	maxCollections = 10000
	// describeSample and describeTimeout bound Describe's sampling.
	describeSample  = 100
	describeTimeout = 5 * time.Second
)

// Namespaces lists the indexes, sorted by name: every index of the project,
// or the one index of an index-host DSN.
func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	if err := c.check(ctx, "namespaces"); err != nil {
		return nil, err
	}
	if c.dsn.host != "" {
		return []driver.Namespace{{Name: c.dsn.index, Label: "index", Default: true}}, nil
	}
	idxs, err := c.client.ListIndexes(ctx)
	if err != nil {
		return nil, c.fail(ctx, "namespaces", err)
	}
	names := make([]string, 0, len(idxs))
	for _, i := range idxs {
		if i != nil {
			names = append(names, i.Name)
		}
	}
	slices.Sort(names)
	names = names[:min(len(names), maxIndexes)]
	out := make([]driver.Namespace, len(names))
	for i, n := range names {
		out[i] = driver.Namespace{Name: n, Label: "index", Default: len(names) == 1}
	}
	return out, nil
}

// Collections lists the namespaces of an index, sorted by name, with their
// record counts. The default namespace is shown as __default__.
func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	if err := c.check(ctx, "collections"); err != nil {
		return nil, err
	}
	h, err := c.index(ctx, "collections", ns.Name)
	if err != nil {
		return nil, err
	}
	info, err := c.stats(ctx, "collections", h)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(info.namespaces))
	for n := range info.namespaces {
		names = append(names, n)
	}
	slices.Sort(names)
	names = names[:min(len(names), maxCollections)]
	out := make([]driver.Collection, len(names))
	for i, n := range names {
		out[i] = driver.Collection{
			Ref:         driver.CollectionRef{Namespace: h.name, Name: n},
			Type:        driver.CollectionCollection,
			RowEstimate: int64(info.namespaces[n]),
		}
	}
	return out, nil
}

// Describe returns the record shape of a namespace: _id, the vector, and
// the metadata keys inferred from a sample of up to 100 records read
// within 5 seconds. A namespace the index does not have is ErrNotFound.
func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	if err := c.check(ctx, "describe"); err != nil {
		return nil, err
	}
	h, info, err := c.collection(ctx, "describe", ref, true)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithTimeout(ctx, describeTimeout)
	defer cancel()
	recs, err := c.sample(sctx, h, ref.Name)
	if err != nil {
		// Only our own 5 s sampling deadline is forgiven: the fields then
		// come from the index alone. Any other failure fails Describe.
		if ctx.Err() != nil || !errors.Is(sctx.Err(), context.DeadlineExceeded) {
			return nil, err
		}
		recs = nil
	}
	sh := newShape(recs, shapeOptions{values: true})
	sample := make([]driver.Row, len(recs))
	for i, r := range recs {
		sample[i], _ = sh.row(r)
	}
	out := &driver.CollectionSchema{
		Ref:         driver.CollectionRef{Namespace: h.name, Name: ref.Name},
		Type:        driver.CollectionCollection,
		PrimaryKey:  []string{idColumn},
		RowEstimate: int64(info.namespaces[ref.Name]),
		SizeBytes:   -1,
		Schemaless:  true,
		Sample:      sample,
		Comment:     indexComment(info),
	}
	out.Fields = append(out.Fields, driver.Field{Name: idColumn, NativeType: "id", Type: driver.TypeString, Comment: "record id"})
	if !info.sparse {
		native := "vector"
		if info.dim > 0 {
			native = fmt.Sprintf("vector(%d)", info.dim)
		}
		out.Fields = append(out.Fields, driver.Field{Name: valuesColumn, NativeType: native, Type: driver.TypeVector, Comment: "dense vector"})
	}
	if info.sparse || sh.sparse {
		out.Fields = append(out.Fields, driver.Field{Name: sparseColumn, NativeType: "sparse vector", Type: driver.TypeObject, Nullable: true,
			Fields: []driver.Field{
				{Name: "indices", NativeType: "list of integers", Type: driver.TypeList},
				{Name: "values", NativeType: "vector", Type: driver.TypeVector},
			}})
	}
	for _, k := range sh.keys {
		out.Fields = append(out.Fields, driver.Field{Name: displayName(k), NativeType: sh.native[k], Type: sh.types[k], Nullable: true})
	}
	return out, nil
}

// indexComment summarises the index: metric, dimension and vector type.
func indexComment(info indexInfo) string {
	metric := info.metric
	if metric == "" {
		metric = "metric unknown"
	}
	if info.sparse {
		return "sparse index, " + metric
	}
	return fmt.Sprintf("dense index, %d dimensions, %s", info.dim, metric)
}

// sample reads the first describeSample records of a namespace.
func (c *conn) sample(ctx context.Context, h *indexHandle, collection string) ([]record, error) {
	ic := h.ns(pineconeNamespace(collection))
	ids, _, err := c.listPage(ctx, "describe", ic, "", describeSample, "")
	if err != nil {
		return nil, err
	}
	return c.fetch(ctx, "describe", ic, ids)
}
