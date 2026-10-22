package milvus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	"github.com/milvus-io/milvus/client/v2/entity"
	"google.golang.org/protobuf/proto"

	"github.com/xavidop/tobira/driver"
)

// vectorMetrics returns the metric_type of each indexed field, upper-cased
// ("L2", "COSINE", "IP", "BM25"…). A collection without indexes has none.
func (c *conn) vectorMetrics(ctx context.Context, op string, s *collSchema) (map[string]string, error) {
	resp, err := c.svc.DescribeIndex(ctx, &milvuspb.DescribeIndexRequest{DbName: s.db, CollectionName: s.name})
	if err := checkRPC(resp, err); err != nil {
		if codeOf(err) == codeIndexNotFound {
			return map[string]string{}, nil
		}
		return nil, mapError(ctx, op, err)
	}
	out := map[string]string{}
	for _, d := range resp.GetIndexDescriptions() {
		for _, kv := range d.GetParams() {
			if kv.GetKey() == "metric_type" {
				out[d.GetFieldName()] = strings.ToUpper(kv.GetValue())
			}
		}
	}
	return out, nil
}

// metricOf is the tobira metric of a Milvus metric_type ("" for the
// others: BM25, HAMMING, JACCARD…).
func metricOf(m string) driver.VectorMetric {
	switch strings.ToUpper(m) {
	case "L2":
		return driver.MetricL2
	case "COSINE":
		return driver.MetricCosine
	case "IP":
		return driver.MetricInnerProduct
	}
	return driver.MetricDefault
}

// distanceFromScore turns Milvus's score into tobira's _distance, which
// ranks nearest first: an L2 score already is a distance, a cosine score
// is the similarity (distance 1 - score), and inner-product and BM25
// scores grow with relevance (distance -score).
func distanceFromScore(metric string, score float32) float64 {
	switch strings.ToUpper(metric) {
	case "COSINE":
		return 1 - float64(score)
	case "IP", "BM25":
		return -float64(score)
	}
	return float64(score)
}

// textCapable reports whether a text query can search f: f is the output
// of a schema function (BM25 or TEXTEMBEDDING), which Milvus runs on the
// query text.
func textCapable(s *collSchema, f *fieldInfo) bool { return s.function(f) != nil }

// pickVector chooses the vector field to search. field names it; with no
// field, the only candidate whose metric is the requested one. A vector
// query searches dense float vectors; a text query searches function
// outputs (BM25 or TEXTEMBEDDING) when the collection has any, else dense
// vectors through the host embedder. A metric other than the field's
// index metric is ErrUnsupported: Milvus fixes it when the index is built.
func pickVector(s *collSchema, metrics map[string]string, field string, metric driver.VectorMetric, byText bool) (*fieldInfo, error) {
	fail := func(kind error, format string, args ...any) (*fieldInfo, error) {
		return nil, driver.Wrap(kind, Name, "similar", fmt.Errorf(format, args...))
	}
	switch metric {
	case driver.MetricDefault, driver.MetricL2, driver.MetricCosine, driver.MetricInnerProduct:
	default:
		return fail(driver.ErrUnsupported, "unknown metric %q", metric)
	}
	fits := func(f *fieldInfo) bool { return metric == driver.MetricDefault || metricOf(metrics[f.name]) == metric }
	dense := func(f *fieldInfo) bool {
		fn := s.function(f)
		return f.dataType == schemapb.DataType_FloatVector && (fn == nil || fn.typ != schemapb.FunctionType_BM25)
	}
	if field != "" {
		f, ok := s.field(field)
		switch {
		case !ok:
			return fail(driver.ErrNotFound, "collection %s has no field %q", s.name, field)
		case !f.isVector():
			return fail(driver.ErrUnsupported, "field %q is not a vector", field)
		case byText && !textCapable(s, f) && f.dataType != schemapb.DataType_FloatVector:
			return fail(driver.ErrUnsupported, "field %q cannot be searched by text", field)
		case !byText && !dense(f):
			return fail(driver.ErrUnsupported, "field %q is not a dense float vector (%s): search it with a native request", field, f.nativeType())
		case !fits(f):
			return fail(driver.ErrUnsupported, "field %q is indexed with the %s metric, not %s", field, metrics[f.name], metric)
		}
		return f, nil
	}
	var candidates []*fieldInfo
	for i := range s.fields {
		f := &s.fields[i]
		if byText && textCapable(s, f) {
			candidates = append(candidates, f)
		}
	}
	if !byText || len(candidates) == 0 {
		candidates = candidates[:0]
		for i := range s.fields {
			if f := &s.fields[i]; dense(f) && (!byText || !textCapable(s, f)) {
				candidates = append(candidates, f)
			}
		}
	}
	var matching []*fieldInfo
	for _, f := range candidates {
		if fits(f) {
			matching = append(matching, f)
		}
	}
	switch {
	case len(candidates) == 0:
		return fail(driver.ErrUnsupported, "collection %s has no vector field to search", s.name)
	case len(matching) == 1:
		return matching[0], nil
	case len(candidates) == 1:
		return fail(driver.ErrUnsupported, "collection %s's vector %q is indexed with the %s metric, not %s", s.name, candidates[0].name, metrics[candidates[0].name], metric)
	case len(matching) == 0:
		return fail(driver.ErrUnsupported, "no vector of collection %s is indexed with the %s metric", s.name, metric)
	}
	names := make([]string, len(matching))
	for i, f := range matching {
		names[i] = f.name
	}
	return fail(driver.ErrSyntax, "collection %s has several vectors (%s): name one", s.name, strings.Join(names, ", "))
}

// checkTextEgress refuses, on a read-only request, a text search that
// Milvus would send to an outside provider: text searched through a
// TEXTEMBEDDING function goes to the embedding provider. BM25 runs inside
// Milvus. Any other function kind is refused too (fail closed).
func checkTextEgress(s *collSchema, f *fieldInfo, readOnly bool) error {
	fn := s.function(f)
	if !readOnly || fn == nil || fn.typ == schemapb.FunctionType_BM25 {
		return nil
	}
	return driver.Wrap(driver.ErrReadOnly, Name, "similar", fmt.Errorf(
		"read-only: field %q is produced by the %s function %q, which sends the query text to an outside embedding provider (egress); search it with a vector, or from a writable connection", f.name, fn.typ, fn.name))
}

// queryVector returns the vector to search, of dim dimensions: q.Vector,
// or q.Text embedded with the host's embedder. Either one must have finite
// components and the right size. An embedder error keeps its kind.
func (c *conn) queryVector(ctx context.Context, q driver.VectorQuery, dim int) ([]float32, error) {
	check := func(vec []float32, what string) ([]float32, error) {
		if len(vec) != dim {
			return nil, driver.Wrap(driver.ErrSyntax, Name, "similar", fmt.Errorf("%s has %d dimensions, the vector searched has %d", what, len(vec), dim))
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

// effectiveK applies the default and the cap to a requested k; warning is
// non-empty when the cap cut it.
func effectiveK(k int) (int, string) {
	switch {
	case k <= 0:
		return defaultK, ""
	case k > maxK:
		return maxK, fmt.Sprintf("k = %d is above the driver's cap: returning the %d nearest entities", k, maxK)
	}
	return k, ""
}

// searchSpec is one single-vector search.
type searchSpec struct {
	op           string
	field        *fieldInfo
	vector       []float32 // or text
	text         string
	topK, offset int
	filter       string
	params       exprParams
	output       []string
	partitions   []string
	consistency  commonpb.ConsistencyLevel
	searchParams map[string]json.RawMessage
	rerank       *rerankSpec
	readOnly     bool
}

// search runs a search and returns its hits, nearest first, each with the
// raw Milvus score.
func (c *conn) search(ctx context.Context, s *collSchema, sp searchSpec) ([]record, error) {
	ph := &commonpb.PlaceholderValue{Tag: "$0"}
	if sp.text != "" {
		ph.Type = commonpb.PlaceholderType_VarChar
		ph.Values = [][]byte{[]byte(sp.text)}
	} else {
		ph.Type = commonpb.PlaceholderType_FloatVector
		ph.Values = [][]byte{entity.FloatVector(sp.vector).Serialize()}
	}
	group, err := proto.Marshal(&commonpb.PlaceholderGroup{Placeholders: []*commonpb.PlaceholderValue{ph}})
	if err != nil {
		return nil, driver.Wrap(driver.ErrSyntax, Name, sp.op, err)
	}
	annParams := "{}"
	if len(sp.searchParams) > 0 {
		b, err := json.Marshal(sp.searchParams)
		if err != nil {
			return nil, driver.Wrap(driver.ErrSyntax, Name, sp.op, err)
		}
		annParams = string(b)
	}
	req := &milvuspb.SearchRequest{
		DbName:             s.db,
		CollectionName:     s.name,
		PartitionNames:     sp.partitions,
		Dsl:                sp.filter,
		DslType:            commonpb.DslType_BoolExprV1,
		SearchInput:        &milvuspb.SearchRequest_PlaceholderGroup{PlaceholderGroup: group},
		OutputFields:       outputNames(sp.output),
		Nq:                 1,
		ConsistencyLevel:   sp.consistency,
		ExprTemplateValues: sp.params,
		SearchParams: []*commonpb.KeyValuePair{
			{Key: "anns_field", Value: sp.field.name},
			{Key: "topk", Value: strconv.Itoa(sp.topK)},
			{Key: "offset", Value: strconv.Itoa(sp.offset)},
			{Key: "params", Value: annParams},
			{Key: "round_decimal", Value: "-1"},
		},
	}
	if sp.rerank != nil {
		req.FunctionScore = &schemapb.FunctionScore{Functions: []*schemapb.FunctionSchema{rerankFunction(sp.rerank)}}
	}
	var recs []record
	err = c.withLoad(ctx, sp.op, s, sp.readOnly, func() error {
		resp, err := c.svc.Search(ctx, req)
		if err := checkRPC(resp, err); err != nil {
			return err
		}
		res := resp.GetResults()
		scores := res.GetScores()
		n := len(scores)
		if tk := res.GetTopks(); len(tk) > 0 {
			n = min(n, int(tk[0]))
		}
		recs, err = decodeFields(res.GetFieldsData(), n)
		if err != nil {
			return err
		}
		if len(recs) < n {
			recs = append(recs, make([]record, n-len(recs))...)
		}
		pk := s.pkField().name
		intIDs, strIDs := res.GetIds().GetIntId().GetData(), res.GetIds().GetStrId().GetData()
		for i := range recs {
			if recs[i].values == nil {
				recs[i].values = map[string]driver.Value{}
			}
			switch {
			case i < len(intIDs):
				recs[i].values[pk] = driver.Int(intIDs[i])
			case i < len(strIDs):
				recs[i].values[pk] = driver.String(strIDs[i])
			}
			recs[i].score = float64(scores[i])
		}
		return nil
	})
	return recs, err
}

// searchOp runs the search op of a native request.
func (c *conn) searchOp(ctx context.Context, s *collSchema, r *request, readOnly bool, limit int, start time.Time) (driver.ResultSet, error) {
	metrics, err := c.vectorMetrics(ctx, "query", s)
	if err != nil {
		return nil, err
	}
	f, err := pickVector(s, metrics, r.AnnsField, driver.MetricDefault, r.Text != "")
	if err != nil {
		return nil, err
	}
	if r.Text != "" {
		if !textCapable(s, f) {
			return nil, driver.Wrap(driver.ErrSyntax, Name, "query", fmt.Errorf("field %q is not produced by a BM25 or TEXTEMBEDDING function, so Milvus cannot search it by text (similarity search embeds text with tobira's embedder)", f.name))
		}
		if err := checkTextEgress(s, f, readOnly); err != nil {
			return nil, err
		}
	}
	k := defaultK
	if r.Limit != nil && *r.Limit > 0 {
		k = int(min(*r.Limit, int64(maxK*10)))
	}
	k, warning := effectiveK(k)
	offset := 0
	if r.Offset != nil {
		offset = int(*r.Offset)
	}
	recs, err := c.search(ctx, s, searchSpec{
		op: "query", field: f, vector: r.Vector, text: r.Text, topK: k, offset: offset,
		filter: r.Filter, params: r.params, output: r.OutputFields, partitions: r.Partitions,
		consistency: r.consistency, searchParams: r.SearchParams, rerank: r.Rerank, readOnly: readOnly,
	})
	if err != nil {
		return nil, err
	}
	sh := newShape(s, staticFields(s, r.OutputFields), scoreColumn, recs)
	dropped := droppedKeys{}
	rs := newPagedResultSet(sh.columns(recs), sh.rows(recs, dropped), nil, limit, start)
	if warning != "" {
		rs.stats.Warnings = []string{warning}
	}
	rs.warn = dropped.warnings
	return rs, nil
}

// Similar returns the k nearest entities with a _distance column, nearest
// first, plus their fields. k defaults to 10 and is capped at 1000. Text
// is embedded by Milvus for a BM25 or TEXTEMBEDDING output field (the
// latter refused on a read-only connection: it is egress), else by the
// host embedder.
func (c *conn) Similar(ctx context.Context, ref driver.CollectionRef, q driver.VectorQuery) (driver.ResultSet, error) {
	start := time.Now()
	if err := c.check(ctx, "similar"); err != nil {
		return nil, err
	}
	k, warning := effectiveK(q.K)
	if len(q.Vector) == 0 && q.Text == "" {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "similar", errors.New("give a query vector or a text"))
	}
	// Resolve the collection, its vector and the filter before embedding,
	// so a typo never costs an embedder call.
	s, err := c.getSchema(ctx, "similar", c.db(ref.Namespace), ref.Name)
	if err != nil {
		return nil, err
	}
	metrics, err := c.vectorMetrics(ctx, "similar", s)
	if err != nil {
		return nil, err
	}
	f, err := pickVector(s, metrics, q.Field, q.Metric, len(q.Vector) == 0)
	if err != nil {
		return nil, err
	}
	params := exprParams{}
	filter, err := buildFilter(q.Filter, s, c.readOnly, params)
	if err != nil {
		return nil, err
	}
	sp := searchSpec{op: "similar", field: f, topK: k, filter: filter, params: params, consistency: commonpb.ConsistencyLevel_Strong, readOnly: c.readOnly}
	switch {
	case len(q.Vector) == 0 && textCapable(s, f):
		if err := checkTextEgress(s, f, c.readOnly); err != nil {
			return nil, err
		}
		sp.text = q.Text
	default:
		if sp.vector, err = c.queryVector(ctx, q, f.dim); err != nil {
			return nil, err
		}
	}
	recs, err := c.search(ctx, s, sp)
	if err != nil {
		return nil, err
	}
	metric := metrics[f.name]
	for i := range recs {
		recs[i].score = distanceFromScore(metric, float32(recs[i].score))
	}
	sh := newShape(s, staticFields(s, nil), distanceColumn, recs)
	dropped := droppedKeys{}
	rows := sh.rows(recs, dropped)
	st := driver.Stats{RowsAffected: -1, Duration: time.Since(start), Warnings: dropped.warnings()}
	if warning != "" {
		st.Warnings = append(st.Warnings, warning)
	}
	return driver.NewSliceResultSet(sh.columns(recs), rows, st), nil
}

// rerankFunction is the reranker as a FunctionSchema, its params in key
// order so the request is the same every time.
func rerankFunction(r *rerankSpec) *schemapb.FunctionSchema {
	fn := &schemapb.FunctionSchema{Name: "tobira_rerank", Type: schemapb.FunctionType_Rerank, InputFieldNames: r.InputFields}
	keys := make([]string, 0, len(r.Params))
	for k := range r.Params {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		raw := r.Params[k]
		var str string
		if json.Unmarshal(raw, &str) != nil {
			str = string(raw)
		}
		fn.Params = append(fn.Params, &commonpb.KeyValuePair{Key: k, Value: str})
	}
	return fn
}
