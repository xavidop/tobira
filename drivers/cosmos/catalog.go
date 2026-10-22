package cosmos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"

	"github.com/xavidop/tobira/driver"
)

// maxListing caps a Namespaces or Collections listing (a variable so tests
// can lower it).
var maxListing = 10000

const (
	// sampleSize and sampleTimeout bound Describe's sample.
	sampleSize    = 100
	sampleTimeout = 5 * time.Second
	// sampleRows is how many sampled items Describe keeps in Sample.
	sampleRows = 20
	// propsTTL is how long container properties stay cached.
	propsTTL = 60 * time.Second
)

// Namespaces lists the account's databases, sorted, with the DSN's
// database as the default.
func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	const op = "namespaces"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	pager := c.client.NewQueryDatabasesPager("SELECT * FROM root", nil)
	var names []string
	for pager.More() && len(names) < maxListing {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, mapError(ctx, op, err)
		}
		for _, d := range page.Databases {
			names = append(names, d.ID)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	if len(names) > maxListing {
		names = names[:maxListing]
	}
	out := make([]driver.Namespace, len(names))
	for i, n := range names {
		out[i] = driver.Namespace{Name: n, Label: "database", Default: n == c.defaultDB}
	}
	return out, nil
}

// Collections lists a database's containers, sorted by name, each with
// its partition key in Comment. A missing database is ErrNotFound.
func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	const op = "collections"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	db, err := c.database(op, ns.Name)
	if err != nil {
		return nil, err
	}
	dbc, err := c.client.NewDatabase(db)
	if err != nil {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, err)
	}
	pager := dbc.NewQueryContainersPager("SELECT * FROM root", nil)
	var out []driver.Collection
	more := false
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, mapError(ctx, op, err)
		}
		for _, p := range page.Containers {
			if len(out) >= maxListing {
				more = true
				break
			}
			c.props.put(db, p.ID, p)
			out = append(out, driver.Collection{
				Ref:         driver.CollectionRef{Namespace: db, Name: p.ID},
				Type:        driver.CollectionCollection,
				RowEstimate: -1,
				Comment:     "partition key " + strings.Join(p.PartitionKeyDefinition.Paths, ", "),
			})
		}
		if more {
			break
		}
	}
	slices.SortFunc(out, func(a, b driver.Collection) int { return strings.Compare(a.Ref.Name, b.Ref.Name) })
	if more {
		last := &out[len(out)-1]
		last.Comment += fmt.Sprintf(" (the list stopped at %d containers)", maxListing)
	}
	return out, nil
}

// keyPath is one field of an item's key: its tobira name and its path.
type keyPath struct{ name, path string }

// keyPaths are the fields that identify an item: the partition key's
// fields (dotted for a nested path), then id unless the partition key is
// id.
func keyPaths(props *azcosmos.ContainerProperties) []keyPath {
	var out []keyPath
	for _, p := range props.PartitionKeyDefinition.Paths {
		out = append(out, keyPath{pkFieldName(p), p})
	}
	if !slices.ContainsFunc(out, func(k keyPath) bool { return k.name == "id" }) {
		out = append(out, keyPath{"id", "/id"})
	}
	return out
}

// keyFields are the names of keyPaths.
func keyFields(props *azcosmos.ContainerProperties) []string {
	var out []string
	for _, k := range keyPaths(props) {
		out = append(out, k.name)
	}
	return out
}

// Describe reads a container's properties and infers its fields from up to
// 100 items (read for at most 5 s and 32 MiB): nested fields for objects
// and objects inside arrays, type unions such as "number|string",
// Nullable for a field some items lack, and the system properties as
// Generated. The key is the partition key's fields plus id. Unique keys,
// composite indexes and vector indexes are listed as Indexes; the
// container's JSON definition is in DDL. A missing container is
// ErrNotFound.
func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	const op = "describe"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	db, err := c.database(op, ref.Namespace)
	if err != nil {
		return nil, err
	}
	ref.Namespace = db
	cc, err := c.container(op, db, ref.Name)
	if err != nil {
		return nil, err
	}
	props, err := c.containerProps(ctx, db, ref.Name, cc)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	sch := &driver.CollectionSchema{
		Ref: ref, Type: driver.CollectionCollection, RowEstimate: -1, SizeBytes: -1, Schemaless: true,
		PrimaryKey: keyFields(props),
	}
	sample, stop, err := sampleItems(ctx, cc)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	sch.Fields = inferFields(sample)
	if !slices.ContainsFunc(sch.Fields, func(f driver.Field) bool { return f.Name == "id" }) {
		sch.Fields = append([]driver.Field{{Name: "id", NativeType: "string", Type: driver.TypeString}}, sch.Fields...)
	}
	markVectorFields(sch.Fields, props)
	sch.Comment = containerComment(props)
	if stop != "" {
		sch.Comment += fmt.Sprintf(" (sampling stopped at %s with %d items)", stop, len(sample))
	}
	cols := make([]driver.Column, len(sch.Fields))
	for i, f := range sch.Fields {
		cols[i].Name = f.Name
	}
	index := columnIndex(cols)
	for _, d := range sample[:min(len(sample), sampleRows)] {
		row, _ := rowOf(driver.ObjectValue(d...), index, len(cols), false)
		sch.Sample = append(sch.Sample, row)
	}
	sch.Indexes = containerIndexes(props)
	if ddl, err := json.MarshalIndent(props, "", "  "); err == nil {
		sch.DDL = string(ddl)
	}
	return sch, nil
}

// containerComment summarises the partition key and vector policy.
func containerComment(props *azcosmos.ContainerProperties) string {
	parts := []string{"partition key " + strings.Join(props.PartitionKeyDefinition.Paths, ", ")}
	if vp := props.VectorEmbeddingPolicy; vp != nil {
		for _, e := range vp.VectorEmbeddings {
			parts = append(parts, fmt.Sprintf("vector %s (%s, %d dims, %s)", e.Path, e.DataType, e.Dimensions, e.DistanceFunction))
		}
	}
	return strings.Join(parts, "; ")
}

// markVectorFields notes the vector embedding policy on the top-level
// fields it covers.
func markVectorFields(fields []driver.Field, props *azcosmos.ContainerProperties) {
	if props.VectorEmbeddingPolicy == nil {
		return
	}
	for _, e := range props.VectorEmbeddingPolicy.VectorEmbeddings {
		name := pkFieldName(e.Path)
		for i := range fields {
			if fields[i].Name == name {
				fields[i].Comment = fmt.Sprintf("vector: %s, %d dims, %s", e.DataType, e.Dimensions, e.DistanceFunction)
			}
		}
	}
}

// containerIndexes lists the item key, unique keys, composite indexes and
// vector indexes.
func containerIndexes(props *azcosmos.ContainerProperties) []driver.Index {
	out := []driver.Index{{Name: "id", Fields: keyFields(props), Unique: true, Primary: true, Method: "partition key + id"}}
	if uk := props.UniqueKeyPolicy; uk != nil {
		for i, k := range uk.UniqueKeys {
			ix := driver.Index{Name: "unique_key_" + strconv.Itoa(i+1), Unique: true, Method: "unique key"}
			for _, p := range k.Paths {
				ix.Fields = append(ix.Fields, pkFieldName(p))
			}
			out = append(out, ix)
		}
	}
	if ip := props.IndexingPolicy; ip != nil {
		for i, comp := range ip.CompositeIndexes {
			ix := driver.Index{Name: "composite_" + strconv.Itoa(i+1), Method: "composite"}
			for _, p := range comp {
				f := pkFieldName(p.Path)
				if p.Order == azcosmos.CompositeIndexDescending {
					f += " desc"
				}
				ix.Fields = append(ix.Fields, f)
			}
			out = append(out, ix)
		}
		for _, v := range ip.VectorIndexes {
			f := pkFieldName(v.Path)
			out = append(out, driver.Index{Name: "vector_" + f, Fields: []string{f}, Method: string(v.Type)})
		}
	}
	return out
}

// sampleItems reads up to sampleSize items, for at most sampleTimeout and
// maxResultBytes. stop names the limit that ended the sample early ("" when
// none did); the items read so far are returned.
func sampleItems(ctx context.Context, cc *azcosmos.ContainerClient) (docs []driver.Object, stop string, err error) {
	sctx, cancel := context.WithTimeout(ctx, sampleTimeout)
	defer cancel()
	pager := cc.NewQueryItemsPager("SELECT TOP "+strconv.Itoa(sampleSize)+" * FROM c", azcosmos.NewPartitionKey(),
		&azcosmos.QueryOptions{PageSizeHint: sampleSize})
	held := 0
	for pager.More() && len(docs) < sampleSize {
		page, err := pager.NextPage(sctx)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return docs, sampleTimeout.String(), nil
			}
			return nil, "", err
		}
		for _, raw := range page.Items {
			v, err := decodeValue(raw)
			if err != nil {
				continue
			}
			if o, ok := v.AsObject(); ok && len(docs) < sampleSize {
				docs = append(docs, o)
			}
			if held += len(raw); held >= maxResultBytes {
				return docs, budgetText(), nil
			}
		}
	}
	return docs, "", nil
}

// containerProps returns a container's properties, cached for propsTTL.
func (c *conn) containerProps(ctx context.Context, db, name string, cc *azcosmos.ContainerClient) (*azcosmos.ContainerProperties, error) {
	if p, ok := c.props.lookup(db, name); ok {
		return p, nil
	}
	resp, err := cc.Read(ctx, nil)
	if err != nil {
		return nil, err
	}
	if resp.ContainerProperties == nil {
		return nil, errors.New("the container read returned no properties")
	}
	c.props.put(db, name, *resp.ContainerProperties)
	p, _ := c.props.lookup(db, name)
	return p, nil
}

// propsCache keeps container properties for propsTTL.
type propsCache struct {
	mu      sync.Mutex
	entries map[string]propsEntry
}

type propsEntry struct {
	props azcosmos.ContainerProperties
	at    time.Time
}

func newPropsCache() *propsCache { return &propsCache{entries: map[string]propsEntry{}} }

func (p *propsCache) lookup(db, name string) (*azcosmos.ContainerProperties, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[db+"\x00"+name]
	if !ok || time.Since(e.at) > propsTTL {
		return nil, false
	}
	props := e.props
	return &props, true
}

func (p *propsCache) put(db, name string, props azcosmos.ContainerProperties) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries[db+"\x00"+name] = propsEntry{props: props, at: time.Now()}
}

func (p *propsCache) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.entries)
}
