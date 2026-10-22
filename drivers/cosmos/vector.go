package cosmos

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"

	"github.com/xavidop/tobira/driver"
)

const (
	// defaultK is Similar's neighbour count when VectorQuery.K is 0.
	defaultK = 10
	// maxK caps VectorQuery.K.
	maxK = 1000
)

// Cosmos DB's distance functions.
const (
	fnEuclidean  = "euclidean"
	fnCosine     = "cosine"
	fnDotProduct = "dotproduct"
)

// Similar ranks a container's items by VectorDistance to the query vector
// (or to VectorQuery.Text embedded with the host's embedder). The vector
// field is VectorQuery.Field, or the container's only vector embedding
// path; a container without a vector embedding policy is ErrUnsupported.
// The metric defaults to the policy's distance function (cosine when the
// account does not report the policy); another metric, or an unreported
// policy, runs as an exact (brute-force) search. Filter narrows the items, as in
// Browse. Rows are the items' fields plus "_distance": the Euclidean
// distance, 1 - cosine similarity, or the negated dot product, nearest
// first.
func (c *conn) Similar(ctx context.Context, ref driver.CollectionRef, q driver.VectorQuery) (driver.ResultSet, error) {
	const op = "similar"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	start := time.Now()
	db, err := c.database(op, ref.Namespace)
	if err != nil {
		return nil, err
	}
	cc, err := c.container(op, db, ref.Name)
	if err != nil {
		return nil, err
	}
	props, err := c.containerProps(ctx, db, ref.Name, cc)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	emb, err := vectorEmbedding(props, q.Field)
	if err != nil {
		return nil, err
	}
	vec := q.Vector
	if len(vec) == 0 {
		if q.Text == "" {
			return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("give a query vector or text"))
		}
		if c.embedder == nil {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New(driver.NeedEmbedderHint))
		}
		vecs, err := c.embedder.Embed(ctx, []string{q.Text})
		if err != nil {
			return nil, mapError(ctx, op, err)
		}
		if len(vecs) != 1 || len(vecs[0]) == 0 {
			return nil, driver.Wrap(nil, Name, op, errors.New("the embedder returned no vector"))
		}
		vec = vecs[0]
	}
	if emb.Dimensions > 0 && len(vec) != int(emb.Dimensions) {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op,
			fmt.Errorf("the query vector has %d dimensions; %s holds %d", len(vec), emb.Path, emb.Dimensions))
	}
	policyFn := string(emb.DistanceFunction)
	fn, err := vectorFunction(q.Metric, policyFn)
	if err != nil {
		return nil, err
	}
	k := q.K
	if k <= 0 {
		k = defaultK
	}
	k = min(k, maxK)
	where, params, err := whereClause(q.Filter, c.readOnly)
	if err != nil {
		return nil, err
	}
	path, err := fieldPath(pkFieldName(emb.Path))
	if err != nil {
		return nil, err
	}
	qv, err := rawJSON(driver.Vector(vec))
	if err != nil {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, err)
	}
	params = append(params, azcosmos.QueryParameter{Name: "@qv", Value: qv})
	sql := vectorSQL(path, fn, fn != policyFn, k, where)
	pager := cc.NewQueryItemsPager(sql, azcosmos.NewPartitionKey(), &azcosmos.QueryOptions{QueryParameters: params, PageSizeHint: int32(k)})
	var docs []driver.Value
	var dists []driver.Value
	var charge float64
	for pager.More() && len(docs) < k {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, mapError(ctx, op, err)
		}
		charge += float64(page.RequestCharge)
		for _, raw := range page.Items {
			v, err := decodeValue(raw)
			if err != nil {
				return nil, driver.Wrap(nil, Name, op, fmt.Errorf("cannot decode an item: %w", err))
			}
			hit, _ := v.AsObject()
			doc, _ := hit.Get("doc")
			score, _ := hit.Get("score")
			s, ok := score.AsFloat()
			if !ok || len(docs) >= k {
				continue
			}
			docs = append(docs, doc)
			dists = append(dists, driver.Float(distanceOf(fn, s)))
		}
	}
	cols := inferColumns(docs)
	_, objects := objectsOf(docs)
	index := columnIndex(cols)
	src := driver.CollectionRef{Namespace: db, Name: ref.Name}
	for i := range cols {
		cols[i].Source = &src
	}
	for _, key := range keyFields(props) {
		if i, ok := index[key]; ok {
			cols[i].Key = true
		}
	}
	cols = append(cols, driver.Column{Name: "_distance", NativeType: "number", Type: driver.TypeFloat})
	rows := make([]driver.Row, len(docs))
	for i, d := range docs {
		row, _ := rowOf(d, index, len(cols)-1, !objects)
		rows[i] = append(row, dists[i])
	}
	st := driver.Stats{RowsAffected: -1, Duration: time.Since(start)}
	if charge > 0 {
		st.Notices = append(st.Notices, "request charge: "+strconv.FormatFloat(charge, 'f', 2, 64)+" RU")
	}
	return driver.NewSliceResultSet(cols, rows, st), nil
}

// vectorEmbedding picks the vector embedding policy entry for field, or
// the only one when field is "". An account that does not report the
// policy (the emulator) still finds the field: from VectorQuery.Field, or
// from the indexing policy's vector indexes; its distance function and
// dimensions are then unknown ("" and 0).
func vectorEmbedding(props *azcosmos.ContainerProperties, field string) (azcosmos.VectorEmbedding, error) {
	const op = "similar"
	var embs []azcosmos.VectorEmbedding
	if props.VectorEmbeddingPolicy != nil {
		embs = props.VectorEmbeddingPolicy.VectorEmbeddings
	}
	if len(embs) == 0 {
		switch {
		case field != "":
			return azcosmos.VectorEmbedding{Path: "/" + strings.ReplaceAll(field, ".", "/")}, nil
		case props.IndexingPolicy != nil && len(props.IndexingPolicy.VectorIndexes) > 0:
			for _, ix := range props.IndexingPolicy.VectorIndexes {
				embs = append(embs, azcosmos.VectorEmbedding{Path: ix.Path})
			}
		default:
			return azcosmos.VectorEmbedding{}, driver.Wrap(driver.ErrUnsupported, Name, op,
				fmt.Errorf("container %q has no vector embedding policy", props.ID))
		}
	}
	if field == "" {
		if len(embs) == 1 {
			return embs[0], nil
		}
		var names []string
		for _, e := range embs {
			names = append(names, pkFieldName(e.Path))
		}
		return azcosmos.VectorEmbedding{}, driver.Wrap(driver.ErrSyntax, Name, op,
			fmt.Errorf("container %q has several vector fields: name one of %s", props.ID, strings.Join(names, ", ")))
	}
	for _, e := range embs {
		if pkFieldName(e.Path) == field {
			return e, nil
		}
	}
	return azcosmos.VectorEmbedding{}, driver.Wrap(driver.ErrNotFound, Name, op,
		fmt.Errorf("container %q has no vector embedding policy for field %q", props.ID, field))
}

// vectorFunction maps a metric to a Cosmos DB distance function; the
// default metric is the policy's.
func vectorFunction(m driver.VectorMetric, policy string) (string, error) {
	switch m {
	case driver.MetricDefault:
		switch policy {
		case "":
			return fnCosine, nil // no reported policy: Cosmos DB's default function
		case fnEuclidean, fnCosine, fnDotProduct:
			return policy, nil
		}
		return "", driver.Wrap(driver.ErrUnsupported, Name, "similar", fmt.Errorf("unknown distance function %q in the vector policy", policy))
	case driver.MetricL2:
		return fnEuclidean, nil
	case driver.MetricCosine:
		return fnCosine, nil
	case driver.MetricInnerProduct:
		return fnDotProduct, nil
	}
	return "", driver.Wrap(driver.ErrUnsupported, Name, "similar", fmt.Errorf("metric %q is not supported (l2, cosine, ip)", m))
}

// distanceOf turns a VectorDistance score into tobira's _distance, lowest
// nearest: euclidean is already a distance; cosine and dot product are
// similarities.
func distanceOf(fn string, score float64) float64 {
	switch fn {
	case fnCosine:
		return 1 - score
	case fnDotProduct:
		return -score
	}
	return score
}

// vectorSQL builds Similar's query: the top k items as {doc, score},
// ordered by VectorDistance (which Cosmos DB sorts most similar first).
func vectorSQL(path, fn string, bruteForce bool, k int, where string) string {
	dist := "VectorDistance(" + path + ", @qv, " + strconv.FormatBool(bruteForce) + ", {'distanceFunction':'" + fn + "'})"
	sql := "SELECT TOP " + strconv.Itoa(k) + " c AS doc, " + dist + " AS score FROM c"
	if where != "" {
		sql += " WHERE " + where
	}
	return sql + " ORDER BY " + dist
}
