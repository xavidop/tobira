package pinecone

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/pinecone-io/go-pinecone/v7/pinecone"

	"github.com/xavidop/tobira/driver"
)

// effectiveK applies the default and the cap to a requested k; warning is
// non-empty when the cap cut it.
func effectiveK(k int) (int, string) {
	switch {
	case k <= 0:
		return defaultK, ""
	case k > maxK:
		return maxK, fmt.Sprintf("k = %d is above the driver's cap: returning the %d nearest records", k, maxK)
	}
	return k, ""
}

// metricOf is the tobira metric of a Pinecone index metric.
func metricOf(m string) driver.VectorMetric {
	switch m {
	case metricEuclidean:
		return driver.MetricL2
	case metricCosine:
		return driver.MetricCosine
	case metricDot:
		return driver.MetricInnerProduct
	}
	return driver.MetricDefault
}

// checkMetric accepts the requested metric when the index uses it (or none
// is requested). Pinecone fixes the metric when an index is created, so
// any other is ErrUnsupported, and so is an index whose metric is unknown
// (an index host that does not report it: set metric= in the DSN).
func checkMetric(index, metric string, want driver.VectorMetric) error {
	switch want {
	case driver.MetricDefault, driver.MetricL2, driver.MetricCosine, driver.MetricInnerProduct:
	default:
		return driver.Wrap(driver.ErrUnsupported, Name, "similar", fmt.Errorf("unknown metric %q", want))
	}
	if metricOf(metric) == driver.MetricDefault {
		return driver.Wrap(driver.ErrUnsupported, Name, "similar", fmt.Errorf("the metric of index %q is unknown: set metric= (cosine, euclidean or dotproduct) in the DSN", index))
	}
	if want != driver.MetricDefault && metricOf(metric) != want {
		return driver.Wrap(driver.ErrUnsupported, Name, "similar", fmt.Errorf("index %q uses the %s metric, not %s (Pinecone fixes it per index)", index, metric, want))
	}
	return nil
}

// distanceFromScore turns Pinecone's score into tobira's _distance, which
// ranks nearest first: a euclidean score already is a distance, a cosine
// score is the similarity (distance 1 - score) and a dot-product score is
// the product (distance -score).
func distanceFromScore(metric string, score float32) float64 {
	switch metric {
	case metricCosine:
		return 1 - float64(score)
	case metricDot:
		return -float64(score)
	}
	return float64(score)
}

// queryVector returns the vector to search, of size dimensions: q.Vector,
// or q.Text embedded with the host's embedder. Either one must have finite
// components and the right size. An embedder error keeps its kind.
func (c *conn) queryVector(ctx context.Context, q driver.VectorQuery, size int) ([]float32, error) {
	check := func(vec []float32, what string) ([]float32, error) {
		if len(vec) != size {
			return nil, driver.Wrap(driver.ErrSyntax, Name, "similar", fmt.Errorf("%s has %d dimensions, the index has %d", what, len(vec), size))
		}
		for _, f := range vec {
			if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
				return nil, driver.Wrap(driver.ErrSyntax, Name, "similar", fmt.Errorf("%s has a non-finite component", what))
			}
		}
		return vec, nil
	}
	if len(q.Vector) > 0 {
		return check(q.Vector, "the query vector")
	}
	if q.Text == "" {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "similar", errors.New("give a query vector or a text"))
	}
	if c.embedder == nil {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "similar", errors.New(driver.NeedEmbedderHint))
	}
	vecs, err := c.embedder.Embed(ctx, []string{q.Text})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
			err = errors.Join(err, ctxErr)
		}
		return nil, driver.Wrap(driver.KindOf(err), Name, "similar", fmt.Errorf("embedding the text: %w", err))
	}
	if len(vecs) != 1 {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "similar", fmt.Errorf("the embedder returned %d vectors for one text", len(vecs)))
	}
	return check(vecs[0], "the embedding")
}

// Similar returns the k nearest records of a namespace with a _distance
// column, nearest first, plus their vectors and metadata. k defaults to 10
// and is capped at 1000. The filter runs on the server as a Pinecone
// metadata filter.
func (c *conn) Similar(ctx context.Context, ref driver.CollectionRef, q driver.VectorQuery) (driver.ResultSet, error) {
	start := time.Now()
	if err := c.check(ctx, "similar"); err != nil {
		return nil, err
	}
	k, warning := effectiveK(q.K)
	if len(q.Vector) == 0 && q.Text == "" {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "similar", errors.New("give a query vector or a text"))
	}
	if q.Field != "" && q.Field != valuesColumn {
		return nil, driver.Wrap(driver.ErrNotFound, Name, "similar", fmt.Errorf("a Pinecone record has one dense vector, %q, not %q", valuesColumn, q.Field))
	}
	filter, err := pineconeFilter(q.Filter)
	if err != nil {
		return nil, err
	}
	// Resolve the index, its metric and the namespace before embedding,
	// so a typo never costs an embedder call.
	h, err := c.index(ctx, "similar", ref.Namespace)
	if err != nil {
		return nil, err
	}
	if h.metric != "" {
		if err := checkMetric(h.name, h.metric, q.Metric); err != nil {
			return nil, err
		}
	}
	_, info, err := c.collection(ctx, "similar", ref, true)
	if err != nil {
		return nil, err
	}
	if info.sparse {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "similar", fmt.Errorf("index %q is sparse: search it with a sparseVector query", h.name))
	}
	if err := checkMetric(h.name, info.metric, q.Metric); err != nil {
		return nil, err
	}
	vec, err := c.queryVector(ctx, q, info.dim)
	if err != nil {
		return nil, err
	}
	resp, err := h.ns(pineconeNamespace(ref.Name)).QueryByVectorValues(ctx, &pinecone.QueryByVectorValuesRequest{
		Vector: vec, TopK: uint32(k), MetadataFilter: filter, IncludeValues: true, IncludeMetadata: true,
	})
	if err != nil {
		return nil, c.fail(ctx, "similar", err)
	}
	recs := scored(resp)
	metric := info.metric
	src := &driver.CollectionRef{Namespace: h.name, Name: ref.Name}
	sh := newShape(recs, shapeOptions{score: distanceColumn, scoreFn: func(s float32) float64 { return distanceFromScore(metric, s) }, values: true, source: src})
	rows := make([]driver.Row, len(recs))
	for i, r := range recs {
		rows[i], _ = sh.row(r)
	}
	st := driver.Stats{RowsAffected: -1, Duration: time.Since(start)}
	if warning != "" {
		st.Warnings = []string{warning}
	}
	st.Warnings = append(st.Warnings, sh.warnings()...)
	return driver.NewSliceResultSet(sh.columns(), rows, st), nil
}
