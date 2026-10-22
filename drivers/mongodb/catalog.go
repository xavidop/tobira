package mongodb

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/xavidop/tobira/driver"
)

// maxCollections caps a Collections listing (a variable so tests can lower
// it).
var maxCollections = 10000

const (
	// sampleSize and sampleTimeout bound Describe's sample.
	sampleSize    = 100
	sampleTimeout = 5 * time.Second
)

// systemDatabases are left out of Namespaces.
var systemDatabases = map[string]bool{"admin": true, "local": true, "config": true}

// Namespaces lists the databases the user may read, without admin, local
// and config. The DSN's database is the default; it is listed even before
// it holds data.
func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	const op = "namespaces"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	names, err := c.client.ListDatabaseNames(ctx, bson.D{},
		options.ListDatabases().SetNameOnly(true).SetAuthorizedDatabases(true))
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	names = slices.DeleteFunc(names, func(n string) bool { return systemDatabases[n] })
	if c.namedDB && !slices.Contains(names, c.defaultDB) {
		names = append(names, c.defaultDB)
	}
	slices.Sort(names)
	out := make([]driver.Namespace, len(names))
	for i, n := range names {
		out[i] = driver.Namespace{Name: n, Label: "database", Default: n == c.defaultDB}
	}
	return out, nil
}

// Collections lists a database's collections and views (system.* left
// out), from listCollections with nameOnly, at most maxCollections of
// them, sorted by name. When more exist, the last one's Comment says the
// list stopped there.
func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	const op = "collections"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	db := c.database(ns.Name)
	cur, err := db.ListCollections(ctx, bson.D{},
		options.ListCollections().SetNameOnly(true).SetAuthorizedCollections(true).SetBatchSize(1000))
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	defer closeCursor(cur)
	var out []driver.Collection
	for len(out) < maxCollections && cur.Next(ctx) {
		name, _ := cur.Current.Lookup("name").StringValueOK()
		if name == "" || strings.HasPrefix(name, "system.") {
			continue
		}
		typ, _ := cur.Current.Lookup("type").StringValueOK()
		out = append(out, driver.Collection{
			Ref:         driver.CollectionRef{Namespace: db.Name(), Name: name},
			Type:        collectionType(typ),
			RowEstimate: -1,
			Comment:     collectionComment(typ),
		})
	}
	more := len(out) >= maxCollections && cur.Next(ctx)
	if err := cur.Err(); err != nil {
		return nil, mapError(ctx, op, err)
	}
	slices.SortFunc(out, func(a, b driver.Collection) int { return strings.Compare(a.Ref.Name, b.Ref.Name) })
	if more {
		last := &out[len(out)-1]
		last.Comment = strings.TrimSpace(last.Comment + fmt.Sprintf(" (the list stopped at %d collections; query listCollections with a filter to see the rest)", maxCollections))
	}
	return out, nil
}

func collectionType(typ string) driver.CollectionType {
	if typ == "view" {
		return driver.CollectionView
	}
	return driver.CollectionCollection
}

func collectionComment(typ string) string {
	if typ == "timeseries" {
		return "time series collection"
	}
	return ""
}

func closeCursor(cur *mongo.Cursor) {
	ctx, cancel := context.WithTimeout(context.Background(), cursorCloseTimeout)
	defer cancel()
	_ = cur.Close(ctx)
}

// Describe infers a collection's fields from a random sample of up to 100
// documents ($sample, then sorted by _id), read for at most 5 s and 32 MiB: nested fields for documents
// and for documents inside arrays, type unions such as "int|string", and
// Nullable for a field some documents lack. The schema is Schemaless and
// carries the sample. A collection's key is _id; its indexes come from
// listIndexes and its row estimate from the collection's metadata. A
// missing collection is ErrNotFound.
func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	const op = "describe"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	db := c.database(ref.Namespace)
	ref.Namespace = db.Name()
	specs, err := db.ListCollectionSpecifications(ctx, bson.D{{Key: "name", Value: ref.Name}})
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	if len(specs) == 0 {
		return nil, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("collection %q not found in database %q", ref.Name, ref.Namespace))
	}
	sch := &driver.CollectionSchema{
		Ref: ref, Type: collectionType(specs[0].Type), Comment: collectionComment(specs[0].Type),
		RowEstimate: -1, SizeBytes: -1, Schemaless: true,
	}
	coll := db.Collection(ref.Name)
	sample, stop, err := sampleDocs(ctx, coll)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	if stop != "" {
		sch.Comment = strings.TrimSpace(sch.Comment + fmt.Sprintf(" (sampling stopped at %s with %d documents)", stop, len(sample)))
	}
	sch.Fields = inferFields(sample)
	cols := make([]driver.Column, len(sch.Fields))
	for i, f := range sch.Fields {
		cols[i].Name = f.Name
	}
	index := columnIndex(cols)
	for _, d := range sample {
		row, _ := rowOf(d, index, len(cols))
		sch.Sample = append(sch.Sample, row)
	}
	if sch.Type == driver.CollectionView {
		return sch, nil
	}
	sch.PrimaryKey = []string{"_id"}
	if sch.Indexes, err = listIndexes(ctx, coll); err != nil {
		return nil, mapError(ctx, op, err)
	}
	if n, err := coll.EstimatedDocumentCount(ctx); err == nil {
		sch.RowEstimate = n
	}
	return sch, nil
}

// sampleDocs reads up to sampleSize random documents, for at most
// sampleTimeout and maxResultBytes. stop names the limit that ended the
// sample early ("" when none did); the documents read so far are returned.
func sampleDocs(ctx context.Context, coll *mongo.Collection) (docs []bson.Raw, stop string, err error) {
	sctx, cancel := context.WithTimeout(ctx, sampleTimeout)
	defer cancel()
	timedOut := func(err error) bool {
		return errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil
	}
	cur, err := coll.Aggregate(sctx, mongo.Pipeline{
		{{Key: "$sample", Value: bson.D{{Key: "size", Value: sampleSize}}}},
		{{Key: "$sort", Value: bson.D{{Key: "_id", Value: 1}}}}, // stable field order for the same sample
	})
	if err != nil {
		if timedOut(err) || mongo.IsTimeout(err) && ctx.Err() == nil {
			return nil, sampleTimeout.String(), nil
		}
		return nil, "", err
	}
	defer closeCursor(cur)
	held := 0
	for len(docs) < sampleSize && cur.Next(sctx) {
		docs = append(docs, append(bson.Raw(nil), cur.Current...))
		if held += len(cur.Current); held >= maxResultBytes {
			return docs, budgetText(), nil
		}
	}
	if err := cur.Err(); err != nil {
		if timedOut(err) || mongo.IsTimeout(err) && ctx.Err() == nil {
			return docs, sampleTimeout.String(), nil
		}
		return nil, "", err
	}
	return docs, "", nil
}

// listIndexes reads a collection's index specifications.
func listIndexes(ctx context.Context, coll *mongo.Collection) ([]driver.Index, error) {
	specs, err := coll.Indexes().ListSpecifications(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]driver.Index, 0, len(specs))
	for _, s := range specs {
		ix := driver.Index{Name: s.Name, Primary: s.Name == "_id_", Unique: s.Name == "_id_" || (s.Unique != nil && *s.Unique)}
		elems, _ := s.KeysDocument.Elements()
		for _, e := range elems {
			ix.Fields = append(ix.Fields, e.Key())
			if m, ok := e.Value().StringValueOK(); ok && ix.Method == "" {
				ix.Method = m // text, 2dsphere, hashed…
			}
		}
		out = append(out, ix)
	}
	return out, nil
}
