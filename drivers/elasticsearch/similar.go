package elasticsearch

import (
	"context"
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"time"

	"github.com/xavidop/tobira/driver"
)

// Bounds on a similarity search.
const (
	defaultK      = 10
	maxK          = 1000
	minCandidates = 100
	maxCandidates = 10000
)

// similarityMetric maps a dense_vector similarity to the metric its kNN
// score ranks by.
var similarityMetric = map[string]driver.VectorMetric{
	"l2_norm":           driver.MetricL2,
	"cosine":            driver.MetricCosine,
	"dot_product":       driver.MetricInnerProduct,
	"max_inner_product": driver.MetricInnerProduct,
}

// fieldNameRE is what a vector field name may hold to be quoted into a
// script.
var fieldNameRE = regexp.MustCompile(`^[A-Za-z0-9_.@\-]+$`)

// Similar finds the nearest documents to a vector, or to text embedded
// with the host's embedder, on a dense_vector field (the only one, or
// q.Field). When the metric is the field's own similarity (or the
// default) and the field is indexed, it runs an approximate kNN search;
// otherwise an exact script_score over every matching document with
// l2norm, cosineSimilarity or dotProduct. The _distance column is the
// Euclidean distance, 1 - cosine similarity, or the negated inner
// product, recovered from the score.
func (c *conn) Similar(ctx context.Context, ref driver.CollectionRef, q driver.VectorQuery) (driver.ResultSet, error) {
	const op = "similar"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if c.fl != flavorElastic {
		return nil, c.errorf(driver.ErrUnsupported, op, "vector search is not supported on OpenSearch")
	}
	start := time.Now()
	if !validTarget(ref.Name) {
		return nil, c.errorf(driver.ErrNotFound, op, "no index %q", ref.Name)
	}
	if !c.allowSystem && systemTarget(ref.Name) {
		return nil, errRefused(c.name(), true, driver.ErrReadOnly, "%s", systemReason(ref.Name))
	}
	m, indices, err := c.mappingOf(ctx, op, ref.Name)
	if err != nil {
		return nil, err
	}
	fieldName, f, err := c.vectorField(op, m, q.Field)
	if err != nil {
		return nil, err
	}
	vec := q.Vector
	if len(vec) == 0 {
		if q.Text == "" {
			return nil, c.errorf(driver.ErrSyntax, op, "a similarity search needs a vector or a text")
		}
		if c.embedder == nil {
			return nil, c.errorf(driver.ErrUnsupported, op, driver.NeedEmbedderHint)
		}
		vs, err := c.embedder.Embed(ctx, []string{q.Text})
		if err != nil {
			return nil, mapError(ctx, c.name(), op, err)
		}
		if len(vs) != 1 {
			return nil, c.errorf(nil, op, "the embedder returned %d vectors for one text", len(vs))
		}
		vec = vs[0]
	}
	if f.dims > 0 && len(vec) != f.dims {
		return nil, c.errorf(driver.ErrSyntax, op, "the vector has %d dimensions, field %q has %d", len(vec), fieldName, f.dims)
	}
	k := q.K
	if k <= 0 {
		k = defaultK
	}
	k = min(k, maxK)
	own := similarityMetric[f.similarity]
	if f.similarity == "" {
		own = driver.MetricCosine // the 8.x default
	}
	metric := q.Metric
	if metric == driver.MetricDefault {
		metric = own
	}
	if !slices.Contains([]driver.VectorMetric{driver.MetricL2, driver.MetricCosine, driver.MetricInnerProduct}, metric) {
		return nil, c.errorf(driver.ErrUnsupported, op, "metric %q", metric)
	}
	filter, err := buildQueryAs(c.name(), op, q.Filter, m, true, c.allowSystem)
	if err != nil {
		return nil, err
	}
	vecJSON, _ := json.Marshal(vec)
	var body map[string]any
	var distance func(hit) driver.Value
	if metric == own && f.indexed {
		knn := map[string]any{
			"field": fieldName, "query_vector": json.RawMessage(vecJSON), "k": k,
			"num_candidates": min(max(minCandidates, 10*k), maxCandidates),
		}
		if len(q.Filter.Conditions) > 0 || q.Filter.Raw != "" {
			knn["filter"] = filter
		}
		body = map[string]any{"knn": knn, "size": k}
		distance = knnDistance(f.similarity)
	} else {
		src, inverse := scoreScript(metric, fieldName)
		body = map[string]any{"size": k, "query": map[string]any{"script_score": map[string]any{
			"query": map[string]any{"bool": map[string]any{
				"filter": []any{filter, map[string]any{"exists": map[string]any{"field": fieldName}}},
			}},
			"script": map[string]any{"source": src, "params": map[string]any{"q": json.RawMessage(vecJSON)}},
		}}}
		distance = inverse
	}
	data, _ := json.Marshal(body)
	raw, err := c.do(ctx, op, "POST", pathOf(ref.Name, "_search"), nil, data)
	if err != nil {
		return nil, err
	}
	var resp searchResponse
	if err := readJSON(raw, &resp); err != nil {
		return nil, c.errorf(nil, op, "unexpected search response")
	}
	src := (*driver.CollectionRef)(nil)
	if singleIndex(ref.Name, indices) {
		src = &driver.CollectionRef{Namespace: defaultNamespace, Name: ref.Name}
	}
	hits := resp.Hits.Hits
	cs := newColumnSet(hits, m, columnOptions{distance: true, score: true, index: src == nil, source: src})
	rows := make([]driver.Row, len(hits))
	for i, h := range hits {
		rows[i] = cs.row(h, distance)
	}
	return driver.NewSliceResultSet(cs.cols, rows, driver.Stats{RowsAffected: -1, Duration: time.Since(start), Warnings: resp.warnings()}), nil
}

// vectorField picks the dense_vector field: name, or the only one.
func (c *conn) vectorField(op string, m *mapping, name string) (string, *field, error) {
	if name != "" {
		f := m.lookup(name)
		if f == nil || f.typ != "dense_vector" {
			return "", nil, c.errorf(driver.ErrNotFound, op, "no dense_vector field %q", name)
		}
		if !fieldNameRE.MatchString(name) {
			return "", nil, c.errorf(driver.ErrUnsupported, op, "vector field name %q", name)
		}
		return name, f, nil
	}
	var found []string
	var walk func(prefix string, m *mapping)
	walk = func(prefix string, m *mapping) {
		for _, n := range m.order {
			f := m.fields[n]
			if f.typ == "dense_vector" {
				found = append(found, prefix+n)
			}
			if f.props != nil && f.typ != "nested" {
				walk(prefix+n+".", f.props)
			}
		}
	}
	walk("", m)
	switch len(found) {
	case 0:
		return "", nil, c.errorf(driver.ErrUnsupported, op, "the index has no dense_vector field")
	case 1:
		return c.vectorField(op, m, found[0])
	}
	return "", nil, c.errorf(driver.ErrSyntax, op, "the index has several dense_vector fields (%v): name one", found)
}

// knnDistance recovers the distance from a kNN score, by similarity:
// l2_norm scores 1/(1+d²), cosine (1+cos)/2, dot_product (1+dot)/2, and
// max_inner_product 1/(1-dot) for a negative dot, dot+1 otherwise.
func knnDistance(similarity string) func(hit) driver.Value {
	return func(h hit) driver.Value {
		if h.Score == nil || *h.Score <= 0 {
			return driver.Null()
		}
		s := *h.Score
		switch similarity {
		case "l2_norm":
			return driver.Float(math.Sqrt(math.Max(0, 1/s-1)))
		case "dot_product":
			return driver.Float(-(2*s - 1))
		case "max_inner_product":
			return driver.Float(-ipFromScore(s))
		default: // cosine
			return driver.Float(1 - (2*s - 1))
		}
	}
}

// ipFromScore inverts the non-negative inner-product score below.
func ipFromScore(s float64) float64 {
	if s >= 1 {
		return s - 1
	}
	return 1 - 1/s
}

// scoreScript is the exact script_score for a metric over field (a name
// fieldNameRE allows, so it can be quoted) and the function recovering the
// distance from its score. Scores must not be negative, hence the shifts.
func scoreScript(metric driver.VectorMetric, field string) (string, func(hit) driver.Value) {
	inv := func(f func(s float64) float64) func(hit) driver.Value {
		return func(h hit) driver.Value {
			if h.Score == nil || *h.Score <= 0 {
				return driver.Null()
			}
			return driver.Float(f(*h.Score))
		}
	}
	quoted := "'" + field + "'"
	switch metric {
	case driver.MetricL2:
		return "1 / (1 + l2norm(params.q, " + quoted + "))", inv(func(s float64) float64 { return 1/s - 1 })
	case driver.MetricCosine:
		return "cosineSimilarity(params.q, " + quoted + ") + 1.0", inv(func(s float64) float64 { return 2 - s })
	}
	return "double d = dotProduct(params.q, " + quoted + "); return d < 0 ? 1 / (1 - d) : d + 1;", inv(func(s float64) float64 { return -ipFromScore(s) })
}
