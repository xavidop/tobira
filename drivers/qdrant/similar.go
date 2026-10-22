package qdrant

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	pb "github.com/qdrant/go-client/qdrant"

	"github.com/xavidop/tobira/driver"
)

// effectiveK applies the default and the cap to a requested k; warning is
// non-empty when the cap cut it.
func effectiveK(k int) (int, string) {
	switch {
	case k <= 0:
		return defaultK, ""
	case k > maxK:
		return maxK, fmt.Sprintf("k = %d is above the driver's cap: returning the %d nearest points", k, maxK)
	}
	return k, ""
}

// metricOf is the tobira metric of a Qdrant distance ("" for Manhattan,
// which has none).
func metricOf(d pb.Distance) driver.VectorMetric {
	switch d {
	case pb.Distance_Euclid:
		return driver.MetricL2
	case pb.Distance_Cosine:
		return driver.MetricCosine
	case pb.Distance_Dot:
		return driver.MetricInnerProduct
	}
	return driver.MetricDefault
}

// distanceFromScore turns Qdrant's score into tobira's _distance, which
// ranks nearest first: Euclid and Manhattan scores already are distances,
// a cosine score is the similarity (distance 1 - score) and a dot score is
// the product (distance -score).
func distanceFromScore(d pb.Distance, score float32) float64 {
	switch d {
	case pb.Distance_Cosine:
		return 1 - float64(score)
	case pb.Distance_Dot:
		return -float64(score)
	}
	return float64(score)
}

// pickVector chooses the dense vector to search. Field names it (the
// default vector is also "vector"); with no Field, the collection's only
// dense vector, or else the only one whose distance is the requested
// metric. A metric other than the vector's own distance is ErrUnsupported:
// Qdrant fixes the distance per vector when the collection is created.
func pickVector(s *collSchema, field string, metric driver.VectorMetric) (vectorInfo, error) {
	fail := func(kind error, format string, args ...any) (vectorInfo, error) {
		return vectorInfo{}, driver.Wrap(kind, Name, "similar", fmt.Errorf(format, args...))
	}
	switch metric {
	case driver.MetricDefault, driver.MetricL2, driver.MetricCosine, driver.MetricInnerProduct:
	default:
		return fail(driver.ErrUnsupported, "unknown metric %q", metric)
	}
	fits := func(v vectorInfo) bool { return metric == driver.MetricDefault || metricOf(v.distance) == metric }
	if field != "" {
		v, ok := s.vectorByColumn(field)
		if !ok {
			return fail(driver.ErrNotFound, "collection %s has no vector %q", s.ref.Name, field)
		}
		if v.sparse || v.multi {
			return fail(driver.ErrUnsupported, "vector %q is not a dense vector", field)
		}
		if !fits(v) {
			return fail(driver.ErrUnsupported, "vector %q uses %s distance, not %s", field, v.distance, metric)
		}
		return v, nil
	}
	var dense, matching []vectorInfo
	for _, v := range s.vectors {
		if v.sparse || v.multi {
			continue
		}
		dense = append(dense, v)
		if fits(v) {
			matching = append(matching, v)
		}
	}
	switch {
	case len(dense) == 0:
		return fail(driver.ErrUnsupported, "collection %s has no dense vector to search", s.ref.Name)
	case len(dense) == 1 && len(matching) == 0:
		return fail(driver.ErrUnsupported, "collection %s uses %s distance, not %s", s.ref.Name, dense[0].distance, metric)
	case len(matching) == 1:
		return matching[0], nil
	case len(matching) == 0:
		return fail(driver.ErrUnsupported, "no vector of collection %s uses the %s metric", s.ref.Name, metric)
	}
	names := make([]string, len(matching))
	for i, v := range matching {
		names[i] = v.column
	}
	return fail(driver.ErrSyntax, "collection %s has several vectors (%s): name one", s.ref.Name, strings.Join(names, ", "))
}

// queryVector returns the vector to search, of size dimensions: q.Vector,
// or q.Text embedded with the host's embedder. Either one must have finite
// components and the right size. An embedder error keeps its kind.
func (c *conn) queryVector(ctx context.Context, q driver.VectorQuery, size uint64) ([]float32, error) {
	check := func(vec []float32, what string) ([]float32, error) {
		if uint64(len(vec)) != size {
			return nil, driver.Wrap(driver.ErrSyntax, Name, "similar", fmt.Errorf("%s has %d dimensions, the vector searched has %d", what, len(vec), size))
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

// Similar returns the k nearest points with a _distance column, nearest
// first, plus their vectors and payload. k defaults to 10 and is capped at
// 1000.
func (c *conn) Similar(ctx context.Context, ref driver.CollectionRef, q driver.VectorQuery) (driver.ResultSet, error) {
	start := time.Now()
	if err := c.check(ctx, "similar"); err != nil {
		return nil, err
	}
	if err := checkNamespace("similar", ref.Namespace); err != nil {
		return nil, err
	}
	k, warning := effectiveK(q.K)
	if len(q.Vector) == 0 && q.Text == "" {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "similar", errors.New("give a query vector or a text"))
	}
	// Resolve the collection, its vector and the filter before embedding,
	// so a typo never costs an embedder call.
	schema, err := c.getSchema(ctx, "similar", ref.Name)
	if err != nil {
		return nil, err
	}
	v, err := pickVector(schema, q.Field, q.Metric)
	if err != nil {
		return nil, err
	}
	filter, err := buildFilter(q.Filter, schema)
	if err != nil {
		return nil, err
	}
	vec, err := c.queryVector(ctx, q, v.size)
	if err != nil {
		return nil, err
	}
	limit := uint64(k)
	req := &pb.QueryPoints{
		CollectionName: ref.Name,
		Query:          pb.NewQueryDense(vec),
		Filter:         filter,
		Limit:          &limit,
		WithPayload:    pb.NewWithPayload(true),
		WithVectors:    pb.NewWithVectors(true),
	}
	if v.name != "" {
		req.Using = &v.name
	}
	resp, err := c.client.GetPointsClient().Query(ctx, req)
	if err != nil {
		return nil, mapError(ctx, "similar", err)
	}
	pts := scoredPoints(resp.GetResult())
	sh := newShape(schema, pts, true, distanceColumn, func(s float32) float64 { return distanceFromScore(v.distance, s) })
	rows := make([]driver.Row, len(pts))
	for i, p := range pts {
		rows[i], _ = sh.row(p)
	}
	st := driver.Stats{RowsAffected: -1, Duration: time.Since(start)}
	if warning != "" {
		st.Warnings = []string{warning}
	}
	return driver.NewSliceResultSet(sh.columns(pts), rows, st), nil
}
