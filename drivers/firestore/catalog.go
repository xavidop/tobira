package firestore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/api/iterator"

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

// Namespaces lists the connection's one database: the client library
// cannot list databases (that is the Firestore Admin API), and a
// connection is bound to the database its DSN names.
func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	if err := c.check(ctx, "namespaces"); err != nil {
		return nil, err
	}
	return []driver.Namespace{{Name: c.dsn.database, Label: "database", Default: true}}, nil
}

// Collections lists the database's root collections, sorted by name, at
// most maxCollections of them. Subcollections are not listed (finding them
// means visiting every document); open one by its path, such as
// users/ada/orders. When more root collections exist, the last one's
// Comment says the list stopped there.
func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	const op = "collections"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if err := c.checkNamespace(op, ns.Name); err != nil {
		return nil, err
	}
	it := c.client.Collections(ctx)
	var out []driver.Collection
	more := false
	for {
		ref, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, c.fail(ctx, op, err)
		}
		if len(out) >= maxCollections {
			more = true
			break
		}
		out = append(out, driver.Collection{
			Ref:         driver.CollectionRef{Namespace: c.dsn.database, Name: ref.ID},
			Type:        driver.CollectionCollection,
			RowEstimate: -1,
		})
	}
	slices.SortFunc(out, func(a, b driver.Collection) int { return strings.Compare(a.Ref.Name, b.Ref.Name) })
	if more {
		last := &out[len(out)-1]
		last.Comment = fmt.Sprintf("the list stopped at %d collections", maxCollections)
	}
	return out, nil
}

// Describe infers a collection's fields from its first 100 documents (in
// document-id order), read for at most 5 s and 32 MiB: nested fields for
// maps and for maps inside arrays, type unions such as "integer|string",
// and Nullable for a field some documents lack. The schema is Schemaless
// and carries the sample; its key is _id, the document id. The name may be
// a subcollection path (users/ada/orders). A collection without documents
// does not exist in Firestore, so it is ErrNotFound.
func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	const op = "describe"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if err := c.checkNamespace(op, ref.Namespace); err != nil {
		return nil, err
	}
	coll, err := c.collection(op, ref.Name)
	if err != nil {
		return nil, err
	}
	ref.Namespace = c.dsn.database
	sctx, cancel := context.WithTimeout(ctx, sampleTimeout)
	defer cancel()
	it := coll.Limit(sampleSize).Documents(sctx)
	defer it.Stop()
	var rows []docRow
	held, cut := 0, false
	for {
		snap, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			if ctx.Err() == nil && sctx.Err() != nil && len(rows) > 0 {
				cut = true // the sample ran out of time: describe what was read
				break
			}
			return nil, c.fail(ctx, op, err)
		}
		d := rowFromSnapshot(snap)
		rows = append(rows, d)
		if held += rowSize(d); held >= maxResultBytes {
			cut = true
			break
		}
	}
	if len(rows) == 0 {
		return nil, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("collection %q has no documents", ref.Name))
	}
	sch := &driver.CollectionSchema{
		Ref:         ref,
		Type:        driver.CollectionCollection,
		Fields:      inferFields(rows),
		PrimaryKey:  []string{idColumn},
		RowEstimate: -1,
		SizeBytes:   -1,
		Schemaless:  true,
	}
	cols := inferColumns(rows, false)
	index := columnIndex(cols)
	for _, d := range rows {
		row, _ := rowOf(d, index, len(cols), false)
		sch.Sample = append(sch.Sample, row)
	}
	if cut {
		sch.Comment = fmt.Sprintf("inferred from the first %d documents (the sample stopped early)", len(rows))
	}
	return sch, nil
}
