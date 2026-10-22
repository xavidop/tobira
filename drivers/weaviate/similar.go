package weaviate

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

// effectiveK applies the default and the cap to a requested k; warning is
// non-empty when the cap cut it.
func effectiveK(k int) (int, string) {
	switch {
	case k <= 0:
		return defaultK, ""
	case k > maxK:
		return maxK, fmt.Sprintf("k = %d is above the driver's cap: returning the %d nearest objects", k, maxK)
	}
	return k, ""
}

// pickVector chooses the vector to search. Field names its column; with no
// Field, the collection's only vector, or else the only one whose distance
// is the requested metric. A metric other than the vector's own distance
// is ErrUnsupported: Weaviate fixes the distance per vector index.
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
			return fail(driver.ErrNotFound, "collection %s has no vector %q", s.class, field)
		}
		if v.multi {
			return fail(driver.ErrUnsupported, "vector %q is a multi-vector", field)
		}
		if !fits(v) {
			return fail(driver.ErrUnsupported, "vector %q uses %s distance, not %s", field, v.distance, metric)
		}
		return v, nil
	}
	var dense, matching []vectorInfo
	for _, v := range s.vectors {
		if v.multi {
			continue
		}
		dense = append(dense, v)
		if fits(v) {
			matching = append(matching, v)
		}
	}
	switch {
	case len(dense) == 0:
		return fail(driver.ErrUnsupported, "collection %s has no vector to search", s.class)
	case len(dense) == 1 && len(matching) == 0:
		return fail(driver.ErrUnsupported, "collection %s uses %s distance, not %s", s.class, dense[0].distance, metric)
	case len(matching) == 1:
		return matching[0], nil
	case len(matching) == 0:
		return fail(driver.ErrUnsupported, "no vector of collection %s uses the %s metric", s.class, metric)
	}
	names := make([]string, len(matching))
	for i, v := range matching {
		names[i] = v.column
	}
	return fail(driver.ErrSyntax, "collection %s has several vectors (%s): name one", s.class, strings.Join(names, ", "))
}

// queryVector returns the vector to search: q.Vector, or q.Text embedded
// with the host's embedder (never a Weaviate vectorizer: that would be
// egress the user did not ask for). Its components must be finite; its
// size is checked by the server. An embedder error keeps its kind.
func (c *conn) queryVector(ctx context.Context, q driver.VectorQuery) ([]float32, error) {
	check := func(vec []float32, what string) ([]float32, error) {
		if len(vec) == 0 {
			return nil, driver.Wrap(driver.ErrSyntax, Name, "similar", fmt.Errorf("%s is empty", what))
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

// Similar returns the k nearest objects with a _distance column, nearest
// first, plus their properties and vectors. k defaults to 10 and is capped
// at 1000. For an l2-squared vector _distance is the L2 distance.
func (c *conn) Similar(ctx context.Context, ref driver.CollectionRef, q driver.VectorQuery) (driver.ResultSet, error) {
	const op = "similar"
	start := time.Now()
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if err := checkNamespace(op, ref.Namespace); err != nil {
		return nil, err
	}
	k, warning := effectiveK(q.K)
	if len(q.Vector) == 0 && q.Text == "" {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("give a query vector or a text"))
	}
	// Resolve the collection, its vector and the filter before embedding,
	// so a typo never costs an embedder call.
	s, err := c.getSchema(ctx, op, ref.Name)
	if err != nil {
		return nil, err
	}
	v, err := pickVector(s, q.Field, q.Metric)
	if err != nil {
		return nil, err
	}
	where, none, err := buildWhere(q.Filter, s)
	if err != nil {
		return nil, err
	}
	spec := columnSpec{props: s.readable(), vectors: true, extra: distanceColumn, distance: func(d float64) float64 { return distanceOf(v.distance, d) }}
	cols := s.columns(spec)
	st := driver.Stats{RowsAffected: -1}
	if warning != "" {
		st.Warnings = []string{warning}
	}
	if none {
		st.Duration = time.Since(start)
		return driver.NewSliceResultSet(cols, nil, st), nil
	}
	vec, err := c.queryVector(ctx, q)
	if err != nil {
		return nil, err
	}
	near := "{vector: " + floats(vec)
	if v.name != "" {
		near += ", targetVectors: [" + strconv.Quote(v.name) + "]"
	}
	near += "}"
	args := gqlArgs(arg("nearVector", near), arg("where", where), arg("limit", strconv.Itoa(k)))
	objs, err := c.getObjects(ctx, op, s, args, s.selectionOf(spec.props, true, []string{"distance"}))
	if err != nil {
		return nil, err
	}
	rows := make([]driver.Row, len(objs))
	for i, o := range objs {
		rows[i], _ = s.row(spec, o)
	}
	cols[0].Type = idType(rows)
	st.Duration = time.Since(start)
	return driver.NewSliceResultSet(cols, rows, st), nil
}

// floats writes a vector as a GraphQL list literal.
func floats(vec []float32) string {
	parts := make([]string, len(vec))
	for i, f := range vec {
		parts[i] = strconv.FormatFloat(float64(f), 'g', -1, 32)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
