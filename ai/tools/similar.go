package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/firebase/genkit/go/ai"
	"github.com/xavidop/tobira/driver"
)

// NameSimilar is the similarity search tool. It is not in Read: it is
// offered only where a reachable connection is a VectorSearcher (see
// VectorSearchable and SimilarTool).
const NameSimilar = "similar"

// DefaultSimilarK is the number of neighbours similar returns when the
// input sets none (still capped at MaxRows).
const DefaultSimilarK = 10

// SimilarInput is the input of similar. Exactly one of Vector and Text is
// set.
type SimilarInput struct {
	Connection string    `json:"connection" jsonschema:"description=Connection name from list_connections"`
	Namespace  string    `json:"namespace,omitempty" jsonschema:"description=Namespace; empty means the connection default"`
	Collection string    `json:"collection" jsonschema:"description=Collection that holds the vectors"`
	Vector     []float32 `json:"vector,omitempty" jsonschema:"description=Query vector; set this or text but not both"`
	Text       string    `json:"text,omitempty" jsonschema:"description=Query text that the configured embedder turns into a vector; set this or vector but not both"`
	K          int       `json:"k,omitempty" jsonschema:"description=Neighbours to return; default 10 and capped at the configured maximum"`
	Metric     string    `json:"metric,omitempty" jsonschema:"enum=l2,enum=cosine,enum=ip,description=Distance metric; empty means the collection default"`
	// Filter narrows the candidates before ranking.
	Filter *SimilarFilter `json:"filter,omitempty" jsonschema:"description=Conditions the rows must all meet"`
}

// SimilarFilter is similar's filter: conditions that are ANDed.
type SimilarFilter struct {
	Conditions []FilterCondition `json:"conditions,omitempty"`
}

// FilterCondition compares one field with a value.
type FilterCondition struct {
	Field string `json:"field"`
	Op    string `json:"op" jsonschema:"enum=eq,enum=ne,enum=lt,enum=le,enum=gt,enum=ge,enum=contains,enum=prefix,enum=in,enum=is_null,enum=not_null"`
	// Value is ignored by is_null and not_null, and is a list for in.
	Value any `json:"value,omitempty"`
}

// filterOps are the operators a FilterCondition may use.
var filterOps = []driver.Op{
	driver.OpEq, driver.OpNe, driver.OpLt, driver.OpLe, driver.OpGt, driver.OpGe,
	driver.OpContains, driver.OpPrefix, driver.OpIn, driver.OpIsNull, driver.OpNotNull,
}

// similarReport is what a similar call reports as its input: the query
// vector is summarised, since it can hold thousands of numbers.
type similarReport struct {
	Connection string         `json:"connection"`
	Namespace  string         `json:"namespace,omitempty"`
	Collection string         `json:"collection"`
	Vector     string         `json:"vector,omitempty"`
	Text       string         `json:"text,omitempty"`
	K          int            `json:"k,omitempty"`
	Metric     string         `json:"metric,omitempty"`
	Filter     *SimilarFilter `json:"filter,omitempty"`
}

func (in SimilarInput) report() similarReport {
	r := similarReport{Connection: in.Connection, Namespace: in.Namespace, Collection: in.Collection, Text: in.Text, K: in.K, Metric: in.Metric, Filter: in.Filter}
	if len(in.Vector) > 0 {
		r.Vector = vectorSummary(len(in.Vector))
	}
	return r
}

// vectorSummary stands for a vector whose values are not shown.
func vectorSummary(dims int) string { return fmt.Sprintf("<vector dim=%d>", dims) }

// SimilarTool returns the similar tool. The assistant and the MCP server
// offer it only when VectorSearchable.
func (t *Toolset) SimilarTool() ai.ToolRef { return t.similar }

// VectorSearchable reports whether any connection the tools may name in
// ctx's turn has a driver that declares driver.CapVectorSearcher. It opens
// nothing.
func (t *Toolset) VectorSearchable(ctx context.Context) bool {
	if t.deps.Manager == nil {
		return false
	}
	for _, name := range t.deps.Manager.Names() {
		if _, unavailable := t.deps.Unavailable[name]; unavailable {
			continue
		}
		if t.allowed(ctx, name) && driver.HasCapability(t.deps.Manager.State(name).Info, driver.CapVectorSearcher) {
			return true
		}
	}
	return false
}

// Similar finds the k nearest rows to a vector or a text, read-only. Its
// rows obey the row policy and are recorded like query's, as
// "similar <collection> k=<k> <"text"|vector dim=N>".
func (t *Toolset) Similar(ctx context.Context, in SimilarInput) (RowsOutput, error) {
	in.Connection = t.scopedConn(ctx, in.Connection)
	return run(t, ctx, NameSimilar, in.Connection, in.report(), func(call, turn context.Context) (RowsOutput, error) {
		if strings.TrimSpace(in.Collection) == "" {
			return RowsOutput{}, errors.New("collection is required: call list_collections for the names")
		}
		if (len(in.Vector) == 0) == (in.Text == "") {
			return RowsOutput{}, errors.New("set exactly one of vector or text")
		}
		metric := driver.VectorMetric(in.Metric)
		switch metric {
		case driver.MetricDefault, driver.MetricL2, driver.MetricCosine, driver.MetricInnerProduct:
		default:
			return RowsOutput{}, errors.New("metric must be l2, cosine or ip, or empty for the collection default")
		}
		filter, err := in.Filter.driverFilter()
		if err != nil {
			return RowsOutput{}, err
		}
		c, err := t.openReadable(call, in.Connection)
		if err != nil {
			return RowsOutput{}, err
		}
		vs, ok := driver.As[driver.VectorSearcher](c)
		if !ok {
			return RowsOutput{}, fmt.Errorf("similarity search is not supported by %s", t.deps.Manager.State(in.Connection).Info.Name)
		}
		k := in.K
		if k <= 0 {
			k = DefaultSimilarK
		}
		capped := k > t.maxRows
		k = min(k, t.maxRows)
		ref := driver.CollectionRef{Namespace: in.Namespace, Name: in.Collection}
		query := fmt.Sprintf("%q", in.Text)
		if len(in.Vector) > 0 {
			query = "vector dim=" + fmt.Sprint(len(in.Vector))
		}
		text := fmt.Sprintf("similar %s k=%d %s", qualified(ref), k, query)
		q := driver.VectorQuery{Vector: in.Vector, Text: in.Text, K: k, Filter: filter, Metric: metric}
		out, err := t.rows(call, turn, in.Connection, text, t.consentWhat(in.Connection, NameSimilar, text), k, func(ctx context.Context) (driver.ResultSet, error) {
			return vs.Similar(ctx, ref, q)
		})
		if err != nil {
			return RowsOutput{}, embedderHint(err, in.Text != "")
		}
		out.Truncated = out.Truncated || capped
		return out, nil
	})
}

// embedderHint replaces the error of a text query the driver could not
// embed for lack of an embedder with the fixed hint, so the row policy's
// scrubbing (which keeps only the error's kind) does not hide it: the
// hint is tobira's own text, not row data.
func embedderHint(err error, text bool) error {
	if text && errors.Is(err, driver.ErrUnsupported) && strings.Contains(err.Error(), driver.NeedEmbedderHint) {
		return errors.New(driver.NeedEmbedderHint + ": set ai.embedder, or search with a vector")
	}
	return err
}

// driverFilter converts f; nil is no filter.
func (f *SimilarFilter) driverFilter() (driver.Filter, error) {
	var out driver.Filter
	if f == nil {
		return out, nil
	}
	for i, c := range f.Conditions {
		op := driver.Op(c.Op)
		if !slices.Contains(filterOps, op) {
			return driver.Filter{}, fmt.Errorf("filter op %q is not supported: use one of eq, ne, lt, le, gt, ge, contains, prefix, in, is_null, not_null", c.Op)
		}
		if strings.TrimSpace(c.Field) == "" {
			return driver.Filter{}, fmt.Errorf("filter condition %d has no field", i+1)
		}
		v, err := driver.FromAny(wholeNumbers(c.Value))
		if err != nil {
			return driver.Filter{}, fmt.Errorf("filter condition %d: unsupported value", i+1)
		}
		out.Conditions = append(out.Conditions, driver.Condition{Field: c.Field, Op: op, Value: v})
	}
	return out, nil
}

// wholeNumbers turns JSON numbers that are whole into int64, in lists
// too, so an integer field compares with an integer.
func wholeNumbers(x any) any {
	switch v := x.(type) {
	case float64:
		if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
			return int64(v)
		}
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = wholeNumbers(e)
		}
		return out
	}
	return x
}
