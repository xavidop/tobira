package chroma

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/xavidop/tobira/driver"
)

// metricOf maps a Chroma space to a metric ("" is Chroma's default, l2).
func metricOf(space string) (driver.VectorMetric, bool) {
	switch space {
	case "", "l2":
		return driver.MetricL2, true
	case "cosine":
		return driver.MetricCosine, true
	case "ip":
		return driver.MetricInnerProduct, true
	}
	return "", false
}

// convertDistance turns Chroma's distance into the VectorQuery contract:
// Chroma's l2 is the squared distance (the root is the Euclidean one), its
// cosine is already 1 - cosine similarity, and its ip is 1 - inner product
// (the contract is the negated inner product).
func convertDistance(space string, d float64) float64 {
	switch space {
	case "cosine":
		return d
	case "ip":
		return d - 1
	}
	return math.Sqrt(math.Max(d, 0))
}

// embed turns texts into vectors with the host's embedder. tobira never
// asks Chroma (or an embedding function configured on the collection) to
// embed: that would send the text to whatever provider the collection
// names, with credentials tobira does not manage.
func (c *conn) embed(ctx context.Context, op string, texts []string) ([][]float32, error) {
	if c.embedder == nil {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New(driver.NeedEmbedderHint))
	}
	vs, err := c.embedder.Embed(ctx, texts)
	if err != nil {
		var de *driver.Error
		if errors.As(err, &de) {
			return nil, err
		}
		return nil, driver.Wrap(nil, Name, op, fmt.Errorf("embedder: %w", err))
	}
	if len(vs) != len(texts) {
		return nil, driver.Wrap(nil, Name, op, fmt.Errorf("embedder returned %d vectors for %d texts", len(vs), len(texts)))
	}
	return vs, nil
}

// Similar runs a nearest-neighbour query on the collection's embedding.
// Chroma fixes the distance per collection, so another metric is
// ErrUnsupported. Text is embedded with the host embedder after the
// collection, metric and filter are resolved, so a mistake costs no
// embedding call. _distance follows the VectorQuery contract.
func (c *conn) Similar(ctx context.Context, ref driver.CollectionRef, q driver.VectorQuery) (driver.ResultSet, error) {
	start := time.Now()
	if err := c.check(ctx, "similar"); err != nil {
		return nil, err
	}
	if len(q.Vector) == 0 && q.Text == "" {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "similar", errors.New("give a vector or a text"))
	}
	if q.Field != "" && q.Field != embeddingColumn {
		return nil, driver.Wrap(driver.ErrNotFound, Name, "similar", fmt.Errorf("a chroma record has one vector, %q, not %q", embeddingColumn, q.Field))
	}
	info, err := c.collection(ctx, "similar", ref)
	if err != nil {
		return nil, err
	}
	metric, ok := metricOf(info.space)
	if !ok {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "similar", fmt.Errorf("collection %q uses space %q, which tobira does not know", info.ref.Name, info.space))
	}
	if q.Metric != driver.MetricDefault && q.Metric != metric {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "similar", fmt.Errorf("collection %q is indexed for %s distance (space %q); chroma cannot search it by %q", info.ref.Name, metric, info.space, q.Metric))
	}
	w, err := buildWhere(q.Filter)
	if err != nil {
		return nil, err
	}
	k := q.K
	if k <= 0 {
		k = defaultK
	}
	k = min(k, maxK)
	include := []string{"documents", "metadatas", "distances"}
	o := optsFor(info, include)
	if w.none() {
		// An empty id selection matches nothing: no embedding, no query.
		sh := newShape(nil, o)
		return driver.NewSliceResultSet(sh.columns(), nil, driver.Stats{RowsAffected: -1, Duration: time.Since(start)}), nil
	}
	vec := q.Vector
	if len(vec) == 0 {
		vs, err := c.embed(ctx, "similar", []string{q.Text})
		if err != nil {
			return nil, err
		}
		vec = vs[0]
	}
	if err := checkDim(info, vec, "similar"); err != nil {
		return nil, err
	}
	resp, err := c.api.query(ctx, c.tenant, info.ref.Namespace, info.id, queryRequest{
		QueryEmbeddings: [][]float32{vec}, IDs: w.ids, Where: w.where, WhereDocument: w.whereDocument, NResults: k, Include: include,
	})
	if err != nil {
		return nil, c.fail(ctx, "similar", err)
	}
	recs, err := queryRecords(resp, func(d float64) float64 { return convertDistance(info.space, d) })
	if err != nil {
		return nil, driver.Wrap(nil, Name, "similar", err)
	}
	sh := newShape(recs, o)
	rows := make([]driver.Row, len(recs))
	for i, r := range recs {
		rows[i] = sh.row(r)
	}
	return driver.NewSliceResultSet(sh.columns(), rows, driver.Stats{RowsAffected: -1, Duration: time.Since(start), Warnings: sh.warnings()}), nil
}
