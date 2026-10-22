package dynamodb

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/xavidop/tobira/driver"
)

const (
	// maxTables bounds Collections.
	maxTables = 10000
	// catalogTTL is how long table names, key schemas and attribute names
	// stay cached.
	catalogTTL = 60 * time.Second
	// sampleTimeout bounds Describe's sample scan.
	sampleTimeout = 5 * time.Second
)

// tableMeta is the part of a table's description the driver needs to page,
// edit and label results: its key attributes in key order (partition key,
// then sort key) and their declared types (S, N or B).
type tableMeta struct {
	name     string
	keys     []string
	keyTypes map[string]string
}

func metaOf(td *types.TableDescription) *tableMeta {
	m := &tableMeta{name: aws.ToString(td.TableName), keyTypes: map[string]string{}}
	for _, kt := range []types.KeyType{types.KeyTypeHash, types.KeyTypeRange} {
		for _, ks := range td.KeySchema {
			if ks.KeyType == kt {
				m.keys = append(m.keys, aws.ToString(ks.AttributeName))
			}
		}
	}
	for _, ad := range td.AttributeDefinitions {
		m.keyTypes[aws.ToString(ad.AttributeName)] = string(ad.AttributeType)
	}
	return m
}

// checkNamespace accepts the one namespace, or "".
func checkNamespace(ns, op string) error {
	if ns == "" || ns == namespaceName {
		return nil
	}
	return driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("namespace %q: a DynamoDB connection has one namespace, %q", ns, namespaceName))
}

// Namespaces returns the single namespace, "default", labelled with the
// region.
func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	if err := c.check(ctx, "namespaces"); err != nil {
		return nil, err
	}
	return []driver.Namespace{{Name: namespaceName, Label: "region", Default: true, Comment: c.region}}, nil
}

// Collections lists the tables (ListTables), at most 10000, in name order.
func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	const op = "collections"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if err := checkNamespace(ns.Name, op); err != nil {
		return nil, err
	}
	names, more, err := c.listTables(ctx, op)
	if err != nil {
		return nil, err
	}
	out := make([]driver.Collection, len(names))
	for i, n := range names {
		out[i] = driver.Collection{Ref: driver.CollectionRef{Namespace: namespaceName, Name: n}, Type: driver.CollectionTable, RowEstimate: -1}
	}
	if more && len(out) > 0 {
		out[len(out)-1].Comment = fmt.Sprintf("the list stops at %d tables", maxTables)
	}
	return out, nil
}

func (c *conn) listTables(ctx context.Context, op string) (names []string, more bool, err error) {
	in := &dynamodb.ListTablesInput{}
	for {
		out, err := c.db.ListTables(ctx, in)
		if err != nil {
			return nil, false, c.fail(ctx, op, err)
		}
		names = append(names, out.TableNames...)
		if len(names) >= maxTables {
			return names[:maxTables], len(names) > maxTables || out.LastEvaluatedTableName != nil, nil
		}
		if out.LastEvaluatedTableName == nil {
			return names, false, nil
		}
		in.ExclusiveStartTableName = out.LastEvaluatedTableName
	}
}

// Describe reads DescribeTable (keys, indexes, item count and size) and a
// sample of up to 100 items (one Scan, at most 5 s and 32 MiB) for the
// other attributes. The schema is Schemaless and carries the sample.
func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	const op = "describe"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if err := checkNamespace(ref.Namespace, op); err != nil {
		return nil, err
	}
	out, err := c.db.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(ref.Name)})
	if err != nil {
		return nil, c.fail(ctx, op, err)
	}
	td := out.Table
	meta := metaOf(td)
	c.catalog.putMeta(meta)
	items, stop, err := c.sample(ctx, ref.Name)
	if err != nil {
		return nil, err
	}
	fields := inferFields(items, meta)
	rows := make([]driver.Row, len(items))
	for i, it := range items {
		row := make(driver.Row, len(fields))
		for j, f := range fields {
			row[j] = valueOf(it[f.Name])
		}
		rows[i] = row
	}
	sch := &driver.CollectionSchema{
		Ref:         driver.CollectionRef{Namespace: namespaceName, Name: ref.Name},
		Type:        driver.CollectionTable,
		Fields:      fields,
		PrimaryKey:  meta.keys,
		Indexes:     indexesOf(td, meta),
		RowEstimate: -1,
		SizeBytes:   -1,
		Schemaless:  true,
		Sample:      rows,
		Comment:     tableComment(td, len(items), stop),
	}
	if td.ItemCount != nil {
		sch.RowEstimate = *td.ItemCount
	}
	if td.TableSizeBytes != nil {
		sch.SizeBytes = *td.TableSizeBytes
	}
	return sch, nil
}

// sample scans up to limits.sample items for at most sampleTimeout and
// limits.resultBytes. stop names the limit that ended it early ("" when
// none did); the items read so far are returned.
func (c *conn) sample(ctx context.Context, table string) (items []map[string]types.AttributeValue, stop string, err error) {
	sctx, cancel := context.WithTimeout(ctx, sampleTimeout)
	defer cancel()
	in := &dynamodb.ScanInput{TableName: aws.String(table), Limit: aws.Int32(int32(c.limits.sample))}
	held := 0
	for len(items) < c.limits.sample {
		out, err := c.db.Scan(sctx, in)
		if err != nil {
			if ctx.Err() == nil && sctx.Err() != nil {
				return items, "the 5 s sample time", nil
			}
			return nil, "", c.fail(ctx, "describe", err)
		}
		for _, it := range out.Items {
			items = append(items, it)
			if held += itemSize(it); held >= c.limits.resultBytes {
				return items, budgetText(c.limits.resultBytes), nil
			}
		}
		if out.LastEvaluatedKey == nil {
			break
		}
		in.ExclusiveStartKey = out.LastEvaluatedKey
		in.Limit = aws.Int32(int32(c.limits.sample - len(items)))
		if *in.Limit <= 0 {
			break
		}
	}
	return items, "", nil
}

func budgetText(n int) string { return fmt.Sprintf("%d MiB", n>>20) }

func indexesOf(td *types.TableDescription, meta *tableMeta) []driver.Index {
	method := func(keys []string) string {
		if len(keys) > 1 {
			return "hash+range"
		}
		return "hash"
	}
	keysOf := func(ks []types.KeySchemaElement) []string {
		return metaOf(&types.TableDescription{KeySchema: ks}).keys
	}
	out := []driver.Index{{Name: "primary", Fields: meta.keys, Unique: true, Primary: true, Method: method(meta.keys)}}
	for _, g := range td.GlobalSecondaryIndexes {
		k := keysOf(g.KeySchema)
		out = append(out, driver.Index{Name: aws.ToString(g.IndexName), Fields: k, Method: "gsi " + method(k)})
	}
	for _, l := range td.LocalSecondaryIndexes {
		k := keysOf(l.KeySchema)
		out = append(out, driver.Index{Name: aws.ToString(l.IndexName), Fields: k, Method: "lsi " + method(k)})
	}
	return out
}

func tableComment(td *types.TableDescription, sampled int, stop string) string {
	parts := []string{"status " + strings.ToLower(string(td.TableStatus))}
	if td.BillingModeSummary != nil && td.BillingModeSummary.BillingMode == types.BillingModePayPerRequest {
		parts = append(parts, "on-demand")
	} else if td.ProvisionedThroughput != nil {
		parts = append(parts, fmt.Sprintf("provisioned %d RCU / %d WCU",
			aws.ToInt64(td.ProvisionedThroughput.ReadCapacityUnits), aws.ToInt64(td.ProvisionedThroughput.WriteCapacityUnits)))
	}
	if s := td.StreamSpecification; s != nil && aws.ToBool(s.StreamEnabled) {
		parts = append(parts, "stream "+string(s.StreamViewType))
	}
	parts = append(parts, fmt.Sprintf("attributes inferred from %d sampled items", sampled))
	if stop != "" {
		parts = append(parts, "the sample stopped at "+stop)
	}
	return strings.Join(parts, "; ")
}

// catalogCache caches table names, key schemas and attribute names for
// Browse, Query and completion. It is safe for concurrent use.
type catalogCache struct {
	c     *conn
	mu    sync.Mutex
	gen   uint64 // bumped by invalidate, so a slow fill cannot store stale data
	names cached[[]string]
	metas map[string]cached[*tableMeta]
	attrs map[string]cached[[]string]
}

type cached[T any] struct {
	v  T
	at time.Time
}

func (e cached[T]) fresh() bool { return !e.at.IsZero() && time.Since(e.at) < catalogTTL }

func newCatalogCache(c *conn) *catalogCache {
	return &catalogCache{c: c, metas: map[string]cached[*tableMeta]{}, attrs: map[string]cached[[]string]{}}
}

// Invalidate drops the cached catalog.
func (c *conn) Invalidate() { c.catalog.invalidate() }

var _ driver.CatalogInvalidator = (*conn)(nil)

func (cc *catalogCache) invalidate() {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.gen++
	cc.names = cached[[]string]{}
	cc.metas = map[string]cached[*tableMeta]{}
	cc.attrs = map[string]cached[[]string]{}
}

func (cc *catalogCache) putMeta(m *tableMeta) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.metas[m.name] = cached[*tableMeta]{v: m, at: time.Now()}
}

// meta returns a table's key schema, from the cache or DescribeTable. A
// missing table is ErrNotFound.
func (cc *catalogCache) meta(ctx context.Context, op, table string) (*tableMeta, error) {
	cc.mu.Lock()
	e, gen := cc.metas[table], cc.gen
	cc.mu.Unlock()
	if e.fresh() {
		return e.v, nil
	}
	out, err := cc.c.db.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(table)})
	if err != nil {
		return nil, cc.c.fail(ctx, op, err)
	}
	m := metaOf(out.Table)
	cc.mu.Lock()
	if cc.gen == gen {
		cc.metas[table] = cached[*tableMeta]{v: m, at: time.Now()}
	}
	cc.mu.Unlock()
	return m, nil
}

func (cc *catalogCache) tableNames(ctx context.Context) ([]string, error) {
	cc.mu.Lock()
	e, gen := cc.names, cc.gen
	cc.mu.Unlock()
	if e.fresh() {
		return e.v, nil
	}
	names, _, err := cc.c.listTables(ctx, "complete")
	if err != nil {
		return nil, err
	}
	cc.mu.Lock()
	if cc.gen == gen {
		cc.names = cached[[]string]{v: names, at: time.Now()}
	}
	cc.mu.Unlock()
	return names, nil
}

// attributes returns the attribute names Describe finds in a table, or
// nil when it fails (completion is best effort).
func (cc *catalogCache) attributes(ctx context.Context, table string) []string {
	cc.mu.Lock()
	e, gen := cc.attrs[table], cc.gen
	cc.mu.Unlock()
	if e.fresh() {
		return e.v
	}
	sch, err := cc.c.Describe(ctx, driver.CollectionRef{Name: table})
	if err != nil {
		return nil
	}
	names := make([]string, len(sch.Fields))
	for i, f := range sch.Fields {
		names[i] = f.Name
	}
	cc.mu.Lock()
	if cc.gen == gen {
		cc.attrs[table] = cached[[]string]{v: names, at: time.Now()}
	}
	cc.mu.Unlock()
	return names
}
