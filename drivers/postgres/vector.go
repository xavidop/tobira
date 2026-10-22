package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

const (
	// defaultSimilarK is the number of neighbours when VectorQuery.K is not set.
	defaultSimilarK = 10
	// maxSimilarK caps VectorQuery.K, so one search never ranks an unbounded
	// number of rows into its result.
	maxSimilarK = 1000
	// vectorLookupTimeout bounds each catalog lookup vector search makes:
	// the pgvector probe at Open and the index metric lookup.
	vectorLookupTimeout = 5 * time.Second
	distanceColumn      = "_distance"
)

// vectorOperators maps each metric to its pgvector distance operator. All
// three rank nearest first: <-> is the Euclidean distance, <=> is
// 1 - cosine similarity and <#> is the negated inner product, which is
// exactly driver.VectorQuery's _distance convention.
var vectorOperators = map[driver.VectorMetric]string{
	driver.MetricL2:           "<->",
	driver.MetricCosine:       "<=>",
	driver.MetricInnerProduct: "<#>",
}

const vectorSchemaSQL = `select n.nspname from pg_catalog.pg_extension e
join pg_catalog.pg_namespace n on n.oid = e.extnamespace
where e.extname = 'vector'`

// probeVectorSchema returns the schema the pgvector extension lives in, or ""
// when the database does not have it. Any failure counts as "not installed":
// the extension only adds a capability, so it never fails Open.
func probeVectorSchema(ctx context.Context, pool *pgxpool.Pool) string {
	pctx, cancel := context.WithTimeout(ctx, vectorLookupTimeout)
	defer cancel()
	var schema string
	if err := pool.QueryRow(pctx, vectorSchemaSQL).Scan(&schema); err != nil {
		return ""
	}
	return schema
}

// indexOpclassSQL finds the operator class of a whole-table HNSW or IVFFlat
// index whose first column is the given one.
const indexOpclassSQL = `select opc.opcname
from pg_catalog.pg_index i
join pg_catalog.pg_class t on t.oid = i.indrelid
join pg_catalog.pg_namespace n on n.oid = t.relnamespace
join pg_catalog.pg_class ic on ic.oid = i.indexrelid
join pg_catalog.pg_am am on am.oid = ic.relam
join pg_catalog.pg_attribute a on a.attrelid = t.oid and a.attnum = i.indkey[0]
join pg_catalog.pg_opclass opc on opc.oid = i.indclass[0]
where n.nspname = $1 and t.relname = $2 and a.attname = $3
  and am.amname in ('hnsw', 'ivfflat') and i.indpred is null
order by ic.relname
limit 1`

// metricFromOpclass maps a pgvector operator class to its metric, or
// MetricDefault for one Similar does not offer.
func metricFromOpclass(name string) driver.VectorMetric {
	switch name {
	case "vector_l2_ops":
		return driver.MetricL2
	case "vector_cosine_ops":
		return driver.MetricCosine
	case "vector_ip_ops":
		return driver.MetricInnerProduct
	}
	return driver.MetricDefault
}

// Similar ranks the rows of a table or view by their distance to a query
// vector, nearest first, and returns them with a trailing _distance column.
// It needs the pgvector extension (ErrUnsupported otherwise).
//
//   - The vector column is q.Field, or the collection's only vector column.
//   - The metric is q.Metric; when it is not set, the metric of a vector
//     index on the column (so the index can serve the search), else cosine.
//   - K defaults to 10 and is capped at 1000.
//   - q.Text is embedded with ConnConfig.Embedder when q.Vector is empty.
//   - q.Filter applies as in Browse.
//   - The distance column is _distance, or _distance_2 (the first free
//     _distance_N) when the collection has a column of that name.
//
// The collection's schema and index metric are cached for the catalog TTL
// (sqlcomplete.DefaultTTL); Invalidate drops them.
//
// The search always runs through the read-only path (BEGIN READ ONLY ...
// ROLLBACK), on read-write connections too, so nothing in the filter can
// write.
func (c *conn) Similar(ctx context.Context, ref driver.CollectionRef, q driver.VectorQuery) (_ driver.ResultSet, err error) {
	defer func() { err = c.named(err) }()
	const op = "similar"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if c.vectorSchema == "" {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("the vector extension (pgvector) is not installed in this database"))
	}
	vec, err := c.queryVector(ctx, q)
	if err != nil {
		return nil, err
	}
	schema, ok := c.vec.schema(ref)
	if !ok {
		if schema, err = c.Describe(ctx, ref); err != nil {
			return nil, err
		}
		c.vec.putSchema(ref, schema)
	}
	indexed := driver.MetricDefault
	if q.Metric == driver.MetricDefault {
		if f, err := vectorField(schema, q.Field); err == nil {
			indexed = c.indexedMetric(ctx, schema.Ref, f.Name)
		}
	}
	text, params, err := buildSimilar(schema, c.vectorSchema, q, vec, indexed)
	if err != nil {
		return nil, err
	}
	return c.Query(ctx, driver.Query{Text: text, Params: params, Limit: similarK(q.K), ReadOnly: true})
}

// indexedMetric returns the metric of a vector index on the column, or
// MetricDefault when there is none or the lookup fails. The lookup is bounded
// by vectorLookupTimeout; an answer (including "no index") is cached, a
// failure is not.
func (c *conn) indexedMetric(ctx context.Context, ref driver.CollectionRef, column string) driver.VectorMetric {
	if m, ok := c.vec.metric(ref, column); ok {
		return m
	}
	lctx, cancel := context.WithTimeout(ctx, vectorLookupTimeout)
	defer cancel()
	var opclass string
	err := withReadOnlyTx(lctx, c.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(lctx, indexOpclassSQL, ref.Namespace, ref.Name, column).Scan(&opclass)
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return driver.MetricDefault
	}
	m := metricFromOpclass(opclass)
	c.vec.putMetric(ref, column, m)
	return m
}

// vectorCache keeps, per collection, the schema and the index metric of each
// vector column Similar looked up, for ttl (sqlcomplete.DefaultTTL when
// zero). The zero value is ready to use and safe for concurrent use.
type vectorCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[driver.CollectionRef]*vectorEntry
}

type vectorEntry struct {
	schema    *driver.CollectionSchema
	schemaExp time.Time
	metrics   map[string]cachedMetric
}

type cachedMetric struct {
	metric driver.VectorMetric
	exp    time.Time
}

func (v *vectorCache) clock() time.Time {
	if v.now != nil {
		return v.now()
	}
	return time.Now()
}

func (v *vectorCache) expiry() time.Time {
	ttl := v.ttl
	if ttl <= 0 {
		ttl = sqlcomplete.DefaultTTL
	}
	return v.clock().Add(ttl)
}

// entry returns ref's entry, creating it. v.mu must be held.
func (v *vectorCache) entry(ref driver.CollectionRef) *vectorEntry {
	if v.entries == nil {
		v.entries = map[driver.CollectionRef]*vectorEntry{}
	}
	e := v.entries[ref]
	if e == nil {
		e = &vectorEntry{metrics: map[string]cachedMetric{}}
		v.entries[ref] = e
	}
	return e
}

func (v *vectorCache) schema(ref driver.CollectionRef) (*driver.CollectionSchema, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	e := v.entries[ref]
	if e == nil || e.schema == nil || !v.clock().Before(e.schemaExp) {
		return nil, false
	}
	return e.schema, true
}

func (v *vectorCache) putSchema(ref driver.CollectionRef, s *driver.CollectionSchema) {
	exp := v.expiry()
	v.mu.Lock()
	defer v.mu.Unlock()
	e := v.entry(ref)
	e.schema, e.schemaExp = s, exp
}

func (v *vectorCache) metric(ref driver.CollectionRef, column string) (driver.VectorMetric, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	e := v.entries[ref]
	if e == nil {
		return driver.MetricDefault, false
	}
	m, ok := e.metrics[column]
	if !ok || !v.clock().Before(m.exp) {
		return driver.MetricDefault, false
	}
	return m.metric, true
}

func (v *vectorCache) putMetric(ref driver.CollectionRef, column string, m driver.VectorMetric) {
	exp := v.expiry()
	v.mu.Lock()
	defer v.mu.Unlock()
	v.entry(ref).metrics[column] = cachedMetric{metric: m, exp: exp}
}

func (v *vectorCache) invalidate() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.entries = nil
}

// queryVector returns the query's vector, embedding q.Text with the
// connection's Embedder when q.Vector is empty. An Embedder failure keeps the
// host error's kind.
func (c *conn) queryVector(ctx context.Context, q driver.VectorQuery) ([]float32, error) {
	const op = "similar"
	if len(q.Vector) > 0 {
		return q.Vector, nil
	}
	if q.Text == "" {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("a query vector or text is required"))
	}
	if c.embedder == nil {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New(driver.NeedEmbedderHint))
	}
	vecs, err := c.embedder.Embed(ctx, []string{q.Text})
	if err != nil {
		return nil, driver.Wrap(driver.KindOf(err), Name, op, fmt.Errorf("embed text: %w", err))
	}
	if len(vecs) != 1 {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("the embedder returned %d vectors for 1 text", len(vecs)))
	}
	return vecs[0], nil
}

// similarK applies the default and the cap to VectorQuery.K.
func similarK(k int) int {
	if k <= 0 {
		return defaultSimilarK
	}
	return min(k, maxSimilarK)
}

// vectorDims returns the declared dimensions of a vector column from its
// native type ("vector(4)", or "ext.vector(4)" outside the search path), or
// 0 when the column is unsized.
func vectorDims(native string) int {
	rest, ok := strings.CutSuffix(native, ")")
	if !ok {
		return 0
	}
	i := strings.LastIndex(rest, vectorTypeName+"(")
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(rest[i+len(vectorTypeName)+1:])
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// vectorField resolves the vector column: name when given (the exact name,
// else a unique case-insensitive match), otherwise the collection's only
// vector column. Names that differ only in case without an exact match are
// ErrSyntax naming the candidates.
func vectorField(schema *driver.CollectionSchema, name string) (driver.Field, error) {
	const op = "similar"
	var folded, vectors []driver.Field
	for _, f := range schema.Fields {
		if name != "" && f.Name == name {
			if f.Type != driver.TypeVector {
				return driver.Field{}, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("field %q is not a vector column", f.Name))
			}
			return f, nil
		}
		if name != "" && strings.EqualFold(f.Name, name) {
			folded = append(folded, f)
		}
		if f.Type == driver.TypeVector {
			vectors = append(vectors, f)
		}
	}
	names := func(fs []driver.Field) string {
		out := make([]string, len(fs))
		for i, f := range fs {
			out[i] = f.Name
		}
		return strings.Join(out, ", ")
	}
	if name != "" {
		switch len(folded) {
		case 0:
			return driver.Field{}, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("no field %q in %s", name, schema.Ref.Name))
		case 1:
			if folded[0].Type != driver.TypeVector {
				return driver.Field{}, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("field %q is not a vector column", folded[0].Name))
			}
			return folded[0], nil
		}
		return driver.Field{}, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("field %q matches several columns of %s that differ only in case (%s): use the exact name", name, schema.Ref.Name, names(folded)))
	}
	switch len(vectors) {
	case 0:
		return driver.Field{}, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("%s has no vector column", schema.Ref.Name))
	case 1:
		return vectors[0], nil
	}
	return driver.Field{}, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("%s has several vector columns (%s): name one", schema.Ref.Name, names(vectors)))
}

// distanceColumnName returns "_distance", or "_distance_N" for the smallest
// N >= 2 that no column of the collection already uses.
func distanceColumnName(schema *driver.CollectionSchema) string {
	taken := make(map[string]bool, len(schema.Fields))
	for _, f := range schema.Fields {
		taken[f.Name] = true
	}
	if !taken[distanceColumn] {
		return distanceColumn
	}
	for n := 2; ; n++ {
		if name := distanceColumn + "_" + strconv.Itoa(n); !taken[name] {
			return name
		}
	}
}

// buildSimilar renders the nearest-neighbour SELECT. extSchema is the schema
// of the pgvector extension: the operator and the cast are qualified with
// it, so the search works when the extension is outside the search path.
// indexed is the metric used when q.Metric is not set (MetricDefault means
// cosine). The query vector is parameter $1.
func buildSimilar(schema *driver.CollectionSchema, extSchema string, q driver.VectorQuery, vec []float32, indexed driver.VectorMetric) (string, []driver.Value, error) {
	const op = "similar"
	metric := q.Metric
	if metric == driver.MetricDefault {
		metric = indexed
	}
	if metric == driver.MetricDefault {
		metric = driver.MetricCosine
	}
	operator, ok := vectorOperators[metric]
	if !ok {
		return "", nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("metric %q is not supported (use l2, cosine or ip)", q.Metric))
	}
	field, err := vectorField(schema, q.Field)
	if err != nil {
		return "", nil, err
	}
	if len(vec) == 0 {
		return "", nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("the query vector is empty"))
	}
	for _, f := range vec {
		if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
			return "", nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("the query vector holds NaN or infinity"))
		}
	}
	if dims := vectorDims(field.NativeType); dims > 0 && dims != len(vec) {
		return "", nil, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("the query vector has %d dimensions, %s has %d", len(vec), field.Name, dims))
	}

	b := &browseQuery{op: op, schema: schema}
	col := pgx.Identifier{field.Name}.Sanitize()
	ext := pgx.Identifier{extSchema}.Sanitize()
	dist := col + " OPERATOR(" + ext + "." + operator + ") CAST(" + b.bind(driver.Vector(vec)) + " AS " + pgx.Identifier{extSchema, vectorTypeName}.Sanitize() + ")"
	b.where = append(b.where, col+" IS NOT NULL")
	for _, cond := range q.Filter.Conditions {
		c, err := b.field(cond.Field)
		if err != nil {
			return "", nil, err
		}
		if err := b.addCondition(c, cond); err != nil {
			return "", nil, err
		}
	}
	if raw := q.Filter.Raw; strings.TrimSpace(raw) != "" {
		b.where = append(b.where, "("+raw+"\n)")
	}
	text := "SELECT *, " + dist + " AS " + pgx.Identifier{distanceColumnName(schema)}.Sanitize() +
		" FROM " + pgx.Identifier{schema.Ref.Namespace, schema.Ref.Name}.Sanitize() +
		" WHERE " + strings.Join(b.where, " AND ") +
		" ORDER BY " + dist +
		" LIMIT " + strconv.Itoa(similarK(q.K))
	return text, b.params, nil
}
