package milvus

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"

	"github.com/xavidop/tobira/driver"
)

const (
	// maxCollections caps a collection listing.
	maxCollections = 10000
	// describeSample and describeTimeout bound Describe's sampling of the
	// dynamic field.
	describeSample  = 100
	describeTimeout = 5 * time.Second
)

// Namespaces lists the databases. A user who may not list them sees the
// connection's database alone.
func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	if err := c.check(ctx, "namespaces"); err != nil {
		return nil, err
	}
	resp, err := c.svc.ListDatabases(ctx, &milvuspb.ListDatabasesRequest{})
	names := []string{c.defaultDB}
	if err := checkRPC(resp, err); err != nil {
		if mapped := mapError(ctx, "namespaces", err); !errors.Is(mapped, driver.ErrPermission) {
			return nil, mapped
		}
	} else {
		names = resp.GetDbNames()
		if !slices.Contains(names, c.defaultDB) {
			names = append(names, c.defaultDB)
		}
	}
	slices.Sort(names)
	out := make([]driver.Namespace, len(names))
	for i, n := range names {
		out[i] = driver.Namespace{Name: n, Label: "database", Default: n == c.defaultDB}
	}
	return out, nil
}

// Collections lists a database's collections, sorted by name, at most
// maxCollections of them. A collection that is not loaded says so in its
// comment: a read-only connection cannot query it.
func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	if err := c.check(ctx, "collections"); err != nil {
		return nil, err
	}
	db := c.db(ns.Name)
	resp, err := c.svc.ShowCollections(ctx, &milvuspb.ShowCollectionsRequest{DbName: db})
	if err := checkRPC(resp, err); err != nil {
		return nil, mapError(ctx, "collections", err)
	}
	names := resp.GetCollectionNames()
	// The loaded collections: a second listing, best effort (an older
	// server or a restricted user may refuse it; nothing is flagged then).
	var loaded map[string]bool
	if mem, err := c.svc.ShowCollections(ctx, &milvuspb.ShowCollectionsRequest{DbName: db, Type: milvuspb.ShowType_InMemory}); checkRPC(mem, err) == nil {
		loaded = map[string]bool{}
		for _, n := range mem.GetCollectionNames() {
			loaded[n] = true
		}
	} else if ctx.Err() != nil {
		return nil, mapError(ctx, "collections", ctx.Err())
	}
	type entry struct {
		name   string
		loaded int // -1 unknown, 0 no, 1 yes
	}
	entries := make([]entry, len(names))
	for i, n := range names {
		entries[i] = entry{name: n, loaded: -1}
		if loaded != nil {
			entries[i].loaded = 0
			if loaded[n] {
				entries[i].loaded = 1
			}
		}
	}
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.name, b.name) })
	entries = entries[:min(len(entries), maxCollections)]
	out := make([]driver.Collection, len(entries))
	for i, e := range entries {
		out[i] = driver.Collection{Ref: driver.CollectionRef{Namespace: db, Name: e.name}, Type: driver.CollectionCollection, RowEstimate: -1}
		if e.loaded == 0 {
			out[i].Comment = "not loaded"
		}
	}
	return out, nil
}

// Describe returns the static fields, the primary key, the indexes and
// the schema as JSON. With the dynamic field on, its keys are sampled from
// up to 100 entities read within 5 seconds (only when the collection is
// loaded: Describe never loads one).
func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	if err := c.check(ctx, "describe"); err != nil {
		return nil, err
	}
	s, err := c.getSchema(ctx, "describe", c.db(ref.Namespace), ref.Name)
	if err != nil {
		return nil, err
	}
	out := &driver.CollectionSchema{
		Ref:         driver.CollectionRef{Namespace: s.db, Name: s.name},
		Type:        driver.CollectionCollection,
		PrimaryKey:  []string{s.pkField().name},
		RowEstimate: -1,
		SizeBytes:   -1,
		Comment:     s.description,
		DDL:         s.ddl(),
	}
	for i := range s.fields {
		f := &s.fields[i]
		fd := driver.Field{
			Name:          f.name,
			NativeType:    f.nativeType(),
			Type:          f.driverType(),
			Nullable:      f.nullable,
			Default:       f.defaultText,
			AutoIncrement: f.autoID,
			Generated:     f.output != "",
			Comment:       f.description,
		}
		if fn := s.function(f); fn != nil {
			fd.Comment = strings.TrimSpace(fd.Comment + " (output of " + fn.typ.String() + " function " + fn.name + ")")
		}
		out.Fields = append(out.Fields, fd)
	}
	if st, err := c.svc.GetCollectionStatistics(ctx, &milvuspb.GetCollectionStatisticsRequest{DbName: s.db, CollectionName: s.name}); checkRPC(st, err) == nil {
		for _, kv := range st.GetStats() {
			if kv.GetKey() == "row_count" {
				if n, err := strconv.ParseInt(kv.GetValue(), 10, 64); err == nil {
					out.RowEstimate = n
				}
			}
		}
	}
	if idx, err := c.svc.DescribeIndex(ctx, &milvuspb.DescribeIndexRequest{DbName: s.db, CollectionName: s.name}); checkRPC(idx, err) == nil {
		for _, d := range idx.GetIndexDescriptions() {
			method := ""
			for _, kv := range d.GetParams() {
				switch kv.GetKey() {
				case "index_type":
					method = kv.GetValue() + method
				case "metric_type":
					method += " " + kv.GetValue()
				}
			}
			out.Indexes = append(out.Indexes, driver.Index{Name: d.GetIndexName(), Fields: []string{d.GetFieldName()}, Method: strings.TrimSpace(method)})
		}
		slices.SortFunc(out.Indexes, func(a, b driver.Index) int { return strings.Compare(a.Name, b.Name) })
	}
	if s.dynamic {
		out.Schemaless = true
		if err := c.sampleDynamic(ctx, s, out); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, driver.Wrap(nil, Name, "describe", err)
	}
	return out, nil
}

// sampleDynamic adds the dynamic keys of a sample to out's fields. It
// never loads the collection; when it is not loaded, the comment says so
// and only the static fields are listed. A failed sample is forgiven (the
// static fields stand), except the caller's own cancellation.
func (c *conn) sampleDynamic(ctx context.Context, s *collSchema, out *driver.CollectionSchema) error {
	sctx, cancel := context.WithTimeout(ctx, describeTimeout)
	defer cancel()
	req := &milvuspb.QueryRequest{
		DbName:           s.db,
		CollectionName:   s.name,
		OutputFields:     []string{"*"},
		QueryParams:      []*commonpb.KeyValuePair{{Key: "limit", Value: strconv.Itoa(describeSample)}},
		ConsistencyLevel: commonpb.ConsistencyLevel_Bounded,
	}
	resp, err := c.svc.Query(sctx, req)
	if err := checkRPC(resp, err); err != nil {
		if ctx.Err() != nil {
			return mapError(ctx, "describe", err)
		}
		if notLoaded(err) {
			out.Comment = strings.TrimSpace(out.Comment + " (not loaded: dynamic fields are not sampled)")
		}
		return nil
	}
	recs, err := decodeFields(resp.GetFieldsData(), -1)
	if err != nil {
		return nil
	}
	sh := newShape(s, staticFields(s, nil), "", recs)
	cols := sh.columns(recs)
	out.Sample = sh.rows(recs, nil)
	for _, col := range cols[len(sh.static):] {
		out.Fields = append(out.Fields, driver.Field{Name: col.Name, NativeType: "dynamic", Type: col.Type, Nullable: true})
	}
	return nil
}
