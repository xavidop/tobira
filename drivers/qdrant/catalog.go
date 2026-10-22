package qdrant

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	pb "github.com/qdrant/go-client/qdrant"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/xavidop/tobira/driver"
)

const (
	// maxCollections caps a collection listing.
	maxCollections = 10000
	// describeSample and describeTimeout bound Describe's sampling.
	describeSample  = 100
	describeTimeout = 5 * time.Second
)

// Namespaces returns the one namespace, "default".
func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	if err := c.check(ctx, "namespaces"); err != nil {
		return nil, err
	}
	return []driver.Namespace{{Name: defaultNamespace, Label: "instance", Default: true}}, nil
}

// Collections lists the collections, sorted by name, at most
// maxCollections of them.
func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	if err := c.check(ctx, "collections"); err != nil {
		return nil, err
	}
	if err := checkNamespace("collections", ns.Name); err != nil {
		return nil, err
	}
	resp, err := c.client.GetCollectionsClient().List(ctx, &pb.ListCollectionsRequest{})
	if err != nil {
		return nil, mapError(ctx, "collections", err)
	}
	names := make([]string, 0, len(resp.GetCollections()))
	for _, d := range resp.GetCollections() {
		names = append(names, d.GetName())
	}
	slices.Sort(names)
	names = names[:min(len(names), maxCollections)]
	out := make([]driver.Collection, len(names))
	for i, n := range names {
		out[i] = driver.Collection{Ref: driver.CollectionRef{Namespace: defaultNamespace, Name: n}, Type: driver.CollectionCollection, RowEstimate: -1}
	}
	return out, nil
}

// Describe returns the vectors, the payload fields and the payload indexes.
// Payload fields are inferred from a sample of up to 100 points read within
// 5 seconds, plus every field the collection declares or indexes.
func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	if err := c.check(ctx, "describe"); err != nil {
		return nil, err
	}
	if err := checkNamespace("describe", ref.Namespace); err != nil {
		return nil, err
	}
	schema, err := c.getSchema(ctx, "describe", ref.Name)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithTimeout(ctx, describeTimeout)
	defer cancel()
	pts, _, err := c.scrollPage(sctx, ref.Name, nil, nil, describeSample)
	if err != nil {
		// Only our own 5 s sampling deadline is forgiven: the fields then
		// come from the schema alone. Any other failure fails Describe.
		if ctx.Err() != nil || !errors.Is(sctx.Err(), context.DeadlineExceeded) {
			return nil, err
		}
		pts = nil
	}
	sh := newShape(schema, pts, true, "", nil)
	cols := sh.columns(pts)
	sample := make([]driver.Row, len(pts))
	for i, p := range pts {
		sample[i], _ = sh.row(p)
	}
	out := &driver.CollectionSchema{
		Ref:         driver.CollectionRef{Namespace: defaultNamespace, Name: ref.Name},
		Type:        driver.CollectionCollection,
		PrimaryKey:  []string{idColumn},
		RowEstimate: -1,
		SizeBytes:   -1,
		Schemaless:  true,
		Sample:      sample,
		Comment:     "status " + strings.ToLower(schema.info.GetStatus().String()),
	}
	if n := schema.info.PointsCount; n != nil {
		out.RowEstimate = int64(min(*n, 1<<63-1))
	}
	for i, col := range cols {
		f := driver.Field{Name: col.Name, NativeType: col.NativeType, Type: col.Type, Nullable: true}
		switch {
		case col.Name == idColumn:
			f.Nullable = false
			f.Type, f.NativeType = idType(pts)
		case i > 0 && i <= len(schema.vectors):
			// vectors: their column already says it all
		default:
			f.Fields = objectFields(sample, i)
		}
		out.Fields = append(out.Fields, f)
	}
	keys := make([]string, 0, len(schema.indexes))
	for k := range schema.indexes {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		out.Indexes = append(out.Indexes, driver.Index{Name: k, Fields: []string{schema.column(k)}, Method: strings.ToLower(schema.indexes[k].String())})
	}
	if params := schema.info.GetConfig().GetParams(); params != nil {
		if b, err := (protojson.MarshalOptions{Multiline: true, UseProtoNames: true}).Marshal(params); err == nil {
			out.DDL = string(b)
		}
	}
	return out, nil
}

// objectFields infers the nested fields of the Object values in column i
// of rows: the union of their members, each typed and nested in turn.
func objectFields(rows []driver.Row, i int) []driver.Field {
	vals := make([]driver.Value, len(rows))
	for k, r := range rows {
		vals[k] = r[i]
	}
	return membersOf(vals)
}

func membersOf(vals []driver.Value) []driver.Field {
	var names []string
	members := map[string][]driver.Value{}
	for _, v := range vals {
		obj, ok := v.AsObject()
		if !ok {
			continue
		}
		for _, m := range obj {
			if _, seen := members[m.Key]; !seen {
				names = append(names, m.Key)
			}
			members[m.Key] = append(members[m.Key], m.Value)
		}
	}
	var out []driver.Field
	for _, n := range names {
		t, native := inferValues(members[n])
		if native == "" {
			native = t.String()
		}
		out = append(out, driver.Field{Name: n, NativeType: native, Type: t, Nullable: true, Fields: membersOf(members[n])})
	}
	return out
}
