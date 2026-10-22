package convert

import (
	"fmt"

	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/xavidop/tobira/driver"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
)

// QueryToProto encodes q, each parameter under the value limits.
func QueryToProto(q driver.Query) (*pb.Query, error) {
	params, err := ValuesToProto(q.Params)
	if err != nil {
		return nil, fmt.Errorf("query params: %w", err)
	}
	return &pb.Query{Text: q.Text, Params: params, Namespace: q.Namespace, Limit: int64(q.Limit), ReadOnly: q.ReadOnly}, nil
}

// QueryFromProto decodes a query; nil decodes as the zero Query.
func QueryFromProto(p *pb.Query) (driver.Query, error) {
	var params []driver.Value
	if len(p.GetParams()) > 0 {
		var err error
		if params, err = ValuesFromProto(p.GetParams()); err != nil {
			return driver.Query{}, fmt.Errorf("query params: %w", err)
		}
	}
	if err := checkName("query namespace", p.GetNamespace()); err != nil {
		return driver.Query{}, err
	}
	return driver.Query{Text: p.GetText(), Params: params, Namespace: p.GetNamespace(), Limit: int(p.GetLimit()), ReadOnly: p.GetReadOnly()}, nil
}

// FilterToProto encodes a structured filter.
func FilterToProto(f driver.Filter) (*pb.Filter, error) {
	p := &pb.Filter{Raw: f.Raw}
	for i, c := range f.Conditions {
		if err := checkName("condition field", c.Field); err != nil {
			return nil, err
		}
		v, err := ValueToProto(c.Value)
		if err != nil {
			return nil, fmt.Errorf("condition %d: %w", i, err)
		}
		p.Conditions = append(p.Conditions, &pb.Condition{Field: c.Field, Op: string(c.Op), Value: v})
	}
	return p, nil
}

// FilterFromProto decodes a filter; nil decodes as the empty filter. A nil
// condition is ErrInvalid; a condition without a value reads as NULL, the
// value OpIsNull and OpNotNull ignore.
func FilterFromProto(p *pb.Filter) (driver.Filter, error) {
	f := driver.Filter{Raw: p.GetRaw()}
	for i, c := range p.GetConditions() {
		if c == nil {
			return driver.Filter{}, invalidf("condition %d missing", i)
		}
		if err := checkName("condition field", c.GetField()); err != nil {
			return driver.Filter{}, err
		}
		if err := checkName("condition op", c.GetOp()); err != nil {
			return driver.Filter{}, err
		}
		v := driver.Null()
		if c.GetValue() != nil {
			var err error
			if v, err = ValueFromProto(c.GetValue()); err != nil {
				return driver.Filter{}, fmt.Errorf("condition %d: %w", i, err)
			}
		}
		f.Conditions = append(f.Conditions, driver.Condition{Field: c.GetField(), Op: driver.Op(c.GetOp()), Value: v})
	}
	return f, nil
}

// BrowseOptionsToProto encodes Browse options.
func BrowseOptionsToProto(o driver.BrowseOptions) (*pb.BrowseOptions, error) {
	f, err := FilterToProto(o.Filter)
	if err != nil {
		return nil, err
	}
	p := &pb.BrowseOptions{Filter: f, Limit: int64(o.Limit), Cursor: o.Cursor, ReadOnly: o.ReadOnly}
	for _, s := range o.Sort {
		p.Sort = append(p.Sort, &pb.SortKey{Field: s.Field, Desc: s.Desc})
	}
	return p, nil
}

// BrowseOptionsFromProto decodes Browse options; nil decodes as the zero
// options. The cursor is capped at MaxCursorBytes.
func BrowseOptionsFromProto(p *pb.BrowseOptions) (driver.BrowseOptions, error) {
	f, err := FilterFromProto(p.GetFilter())
	if err != nil {
		return driver.BrowseOptions{}, err
	}
	if len(p.GetCursor()) > MaxCursorBytes {
		return driver.BrowseOptions{}, limitf("cursor is %d bytes, over %d", len(p.GetCursor()), MaxCursorBytes)
	}
	o := driver.BrowseOptions{Filter: f, Limit: int(p.GetLimit()), Cursor: p.GetCursor(), ReadOnly: p.GetReadOnly()}
	for i, s := range p.GetSort() {
		if s == nil {
			return driver.BrowseOptions{}, invalidf("sort key %d missing", i)
		}
		if err := checkName("sort field", s.GetField()); err != nil {
			return driver.BrowseOptions{}, err
		}
		o.Sort = append(o.Sort, driver.SortKey{Field: s.GetField(), Desc: s.GetDesc()})
	}
	return o, nil
}

// ExplainOptionsToProto encodes Explain options.
func ExplainOptionsToProto(o driver.ExplainOptions) *pb.ExplainOptions {
	return &pb.ExplainOptions{Analyze: o.Analyze}
}

// ExplainOptionsFromProto decodes Explain options; nil is the zero options.
func ExplainOptionsFromProto(p *pb.ExplainOptions) driver.ExplainOptions {
	return driver.ExplainOptions{Analyze: p.GetAnalyze()}
}

// PlanToProto encodes an Explain result. A nil plan is ErrInvalid.
func PlanToProto(pl *driver.Plan) (*pb.Plan, error) {
	if pl == nil {
		return nil, invalidf("missing plan")
	}
	root, err := planNodeToProto(pl.Root, 0)
	if err != nil {
		return nil, err
	}
	return &pb.Plan{Format: pl.Format, Text: pl.Text, Root: root}, nil
}

func planNodeToProto(n *driver.PlanNode, depth int) (*pb.PlanNode, error) {
	if n == nil {
		return nil, nil
	}
	if depth >= MaxDepth {
		return nil, limitf("plan nests deeper than %d", MaxDepth)
	}
	p := &pb.PlanNode{Label: n.Label, Detail: n.Detail, Cost: n.Cost, Rows: n.Rows}
	for _, c := range n.Children {
		pc, err := planNodeToProto(c, depth+1)
		if err != nil {
			return nil, err
		}
		if pc != nil {
			p.Children = append(p.Children, pc)
		}
	}
	return p, nil
}

// PlanFromProto decodes an Explain result: a nil plan or child node is
// ErrInvalid, and nodes nest at most MaxDepth levels.
func PlanFromProto(p *pb.Plan) (*driver.Plan, error) {
	if p == nil {
		return nil, invalidf("missing plan")
	}
	pl := &driver.Plan{Format: p.GetFormat(), Text: p.GetText()}
	if p.GetRoot() != nil {
		root, err := planNodeFromProto(p.GetRoot(), 0)
		if err != nil {
			return nil, err
		}
		pl.Root = root
	}
	return pl, nil
}

func planNodeFromProto(p *pb.PlanNode, depth int) (*driver.PlanNode, error) {
	if p == nil {
		return nil, invalidf("plan node missing")
	}
	if depth >= MaxDepth {
		return nil, limitf("plan nests deeper than %d", MaxDepth)
	}
	n := &driver.PlanNode{Label: p.GetLabel(), Detail: p.GetDetail(), Cost: p.GetCost(), Rows: p.GetRows()}
	for _, c := range p.GetChildren() {
		cn, err := planNodeFromProto(c, depth+1)
		if err != nil {
			return nil, err
		}
		n.Children = append(n.Children, cn)
	}
	return n, nil
}

// TxOptionsToProto encodes Begin options.
func TxOptionsToProto(o driver.TxOptions) *pb.TxOptions {
	return &pb.TxOptions{ReadOnly: o.ReadOnly, Isolation: o.Isolation}
}

// TxOptionsFromProto decodes Begin options; nil is the zero options.
func TxOptionsFromProto(p *pb.TxOptions) driver.TxOptions {
	return driver.TxOptions{ReadOnly: p.GetReadOnly(), Isolation: p.GetIsolation()}
}

// CompleteRequestToProto builds the Complete request for conn.
func CompleteRequestToProto(conn *pb.ConnHandle, req driver.CompletionRequest) *pb.CompleteRequest {
	return &pb.CompleteRequest{Conn: conn, Text: req.Text, Cursor: int64(req.Cursor), Namespace: req.Namespace}
}

// CompletionRequestFromProto decodes the completion part of a Complete
// request.
func CompletionRequestFromProto(p *pb.CompleteRequest) driver.CompletionRequest {
	return driver.CompletionRequest{Text: p.GetText(), Cursor: int(p.GetCursor()), Namespace: p.GetNamespace()}
}

// CompletionsToProto encodes a Complete result.
func CompletionsToProto(cs []driver.Completion) ([]*pb.Completion, error) {
	out := make([]*pb.Completion, len(cs))
	for i, c := range cs {
		p := &pb.Completion{Label: c.Label, Kind: c.Kind, Detail: c.Detail, Insert: c.Insert}
		if err := checkCompletion(i, p); err != nil {
			return nil, err
		}
		out[i] = p
	}
	return out, nil
}

func checkCompletion(i int, p *pb.Completion) error {
	return checkNames(fmt.Sprintf("completion %d text", i), []string{p.GetLabel(), p.GetKind(), p.GetDetail(), p.GetInsert()})
}

// CompletionsFromProto decodes a Complete result; a nil entry is ErrInvalid.
func CompletionsFromProto(ps []*pb.Completion) ([]driver.Completion, error) {
	var out []driver.Completion
	for i, p := range ps {
		if p == nil {
			return nil, invalidf("completion %d missing", i)
		}
		if err := checkCompletion(i, p); err != nil {
			return nil, err
		}
		out = append(out, driver.Completion{Label: p.GetLabel(), Kind: p.GetKind(), Detail: p.GetDetail(), Insert: p.GetInsert()})
	}
	return out, nil
}

// VectorQueryToProto encodes a Similar query.
func VectorQueryToProto(q driver.VectorQuery) (*pb.VectorQuery, error) {
	f, err := FilterToProto(q.Filter)
	if err != nil {
		return nil, err
	}
	return &pb.VectorQuery{Vector: q.Vector, Text: q.Text, Field: q.Field, K: int64(q.K), Filter: f, Metric: string(q.Metric)}, nil
}

// VectorQueryFromProto decodes a Similar query; nil is the zero query.
func VectorQueryFromProto(p *pb.VectorQuery) (driver.VectorQuery, error) {
	f, err := FilterFromProto(p.GetFilter())
	if err != nil {
		return driver.VectorQuery{}, err
	}
	if err := checkName("vector field", p.GetField()); err != nil {
		return driver.VectorQuery{}, err
	}
	var vec []float32
	if len(p.GetVector()) > 0 {
		vec = p.GetVector()
	}
	return driver.VectorQuery{Vector: vec, Text: p.GetText(), Field: p.GetField(), K: int(p.GetK()), Filter: f, Metric: driver.VectorMetric(p.GetMetric())}, nil
}

// RangeQueryToProto encodes a Range query. A zero bound is left unset (open).
func RangeQueryToProto(q driver.RangeQuery) (*pb.RangeQuery, error) {
	f, err := FilterToProto(q.Filter)
	if err != nil {
		return nil, err
	}
	p := &pb.RangeQuery{Field: q.Field, Aggregate: q.Aggregate, GroupBy: q.GroupBy, Filter: f}
	if !q.Start.IsZero() {
		p.Start = TimeToProto(q.Start)
	}
	if !q.End.IsZero() {
		p.End = TimeToProto(q.End)
	}
	if q.Window != 0 {
		p.Window = durationpb.New(q.Window)
	}
	return p, nil
}

// RangeQueryFromProto decodes a Range query; nil is the zero query, and an
// unset bound is the zero time.
func RangeQueryFromProto(p *pb.RangeQuery) (driver.RangeQuery, error) {
	f, err := FilterFromProto(p.GetFilter())
	if err != nil {
		return driver.RangeQuery{}, err
	}
	if err := checkNames("range name", append([]string{p.GetField(), p.GetAggregate()}, p.GetGroupBy()...)); err != nil {
		return driver.RangeQuery{}, err
	}
	q := driver.RangeQuery{Field: p.GetField(), Aggregate: p.GetAggregate(), GroupBy: strs(p.GetGroupBy()), Filter: f}
	if q.Start, err = TimeFromProto(p.GetStart()); err != nil {
		return driver.RangeQuery{}, fmt.Errorf("range start: %w", err)
	}
	if q.End, err = TimeFromProto(p.GetEnd()); err != nil {
		return driver.RangeQuery{}, fmt.Errorf("range end: %w", err)
	}
	if w := p.GetWindow(); w != nil {
		q.Window = w.AsDuration()
	}
	return q, nil
}

// GraphQueryToProto encodes a Neighbors query.
func GraphQueryToProto(q driver.GraphQuery) (*pb.GraphQuery, error) {
	start, err := ValueToProto(q.Start)
	if err != nil {
		return nil, fmt.Errorf("graph start: %w", err)
	}
	return &pb.GraphQuery{Start: start, Labels: q.Labels, Depth: int64(q.Depth), Limit: int64(q.Limit)}, nil
}

// GraphQueryFromProto decodes a Neighbors query; nil is the zero query and
// an unset start is NULL.
func GraphQueryFromProto(p *pb.GraphQuery) (driver.GraphQuery, error) {
	start := driver.Null()
	if p.GetStart() != nil {
		var err error
		if start, err = ValueFromProto(p.GetStart()); err != nil {
			return driver.GraphQuery{}, fmt.Errorf("graph start: %w", err)
		}
	}
	if err := checkNames("graph label", p.GetLabels()); err != nil {
		return driver.GraphQuery{}, err
	}
	return driver.GraphQuery{Start: start, Labels: strs(p.GetLabels()), Depth: int(p.GetDepth()), Limit: int(p.GetLimit())}, nil
}

// GraphToProto encodes a Neighbors result. A nil graph is ErrInvalid.
func GraphToProto(g *driver.Graph) (*pb.Graph, error) {
	if g == nil {
		return nil, invalidf("missing graph")
	}
	p := &pb.Graph{}
	for i, n := range g.Nodes {
		props, err := objectOrNil(n.Props)
		if err != nil {
			return nil, fmt.Errorf("node %d: %w", i, err)
		}
		p.Nodes = append(p.Nodes, &pb.Node{Id: n.ID, Labels: n.Labels, Props: props})
	}
	for i, e := range g.Edges {
		props, err := objectOrNil(e.Props)
		if err != nil {
			return nil, fmt.Errorf("edge %d: %w", i, err)
		}
		p.Edges = append(p.Edges, &pb.Edge{Id: e.ID, From: e.From, To: e.To, Type: e.Type, Props: props})
	}
	return p, nil
}

// objectOrNil encodes o, leaving a nil Object unset.
func objectOrNil(o driver.Object) (*pb.Object, error) {
	if o == nil {
		return nil, nil
	}
	return ObjectToProto(o)
}

// GraphFromProto decodes a Neighbors result: a nil graph, node or edge is
// ErrInvalid.
func GraphFromProto(p *pb.Graph) (*driver.Graph, error) {
	if p == nil {
		return nil, invalidf("missing graph")
	}
	g := &driver.Graph{}
	for i, n := range p.GetNodes() {
		if n == nil {
			return nil, invalidf("node %d missing", i)
		}
		if err := checkNames("node name", append([]string{n.GetId()}, n.GetLabels()...)); err != nil {
			return nil, err
		}
		props, err := ObjectFromProto(n.GetProps())
		if err != nil {
			return nil, fmt.Errorf("node %d: %w", i, err)
		}
		g.Nodes = append(g.Nodes, driver.Node{ID: n.GetId(), Labels: strs(n.GetLabels()), Props: props})
	}
	for i, e := range p.GetEdges() {
		if e == nil {
			return nil, invalidf("edge %d missing", i)
		}
		if err := checkNames("edge name", []string{e.GetId(), e.GetFrom(), e.GetTo(), e.GetType()}); err != nil {
			return nil, err
		}
		props, err := ObjectFromProto(e.GetProps())
		if err != nil {
			return nil, fmt.Errorf("edge %d: %w", i, err)
		}
		g.Edges = append(g.Edges, driver.Edge{ID: e.GetId(), From: e.GetFrom(), To: e.GetTo(), Type: e.GetType(), Props: props})
	}
	return g, nil
}

// WatchRequestToProto builds the Watch request for conn.
func WatchRequestToProto(conn *pb.ConnHandle, req driver.WatchRequest) *pb.WatchRequest {
	p := &pb.WatchRequest{Conn: conn, Channel: req.Channel}
	if req.Collection != nil {
		p.Collection = CollectionRefToProto(*req.Collection)
	}
	return p
}

// WatchRequestFromProto decodes the selection part of a Watch request.
func WatchRequestFromProto(p *pb.WatchRequest) (driver.WatchRequest, error) {
	if err := checkName("watch channel", p.GetChannel()); err != nil {
		return driver.WatchRequest{}, err
	}
	req := driver.WatchRequest{Channel: p.GetChannel()}
	if p.GetCollection() != nil {
		ref, err := CollectionRefFromProto(p.GetCollection())
		if err != nil {
			return driver.WatchRequest{}, err
		}
		req.Collection = &ref
	}
	return req, nil
}

// EventToProto encodes a change event. A terminal Err travels as an
// ErrorInfo whose message redact masks (nil leaves it as is).
func EventToProto(ev driver.Event, redact func(string) string) (*pb.Event, error) {
	key, err := objectOrNil(ev.Key)
	if err != nil {
		return nil, fmt.Errorf("event key: %w", err)
	}
	payload, err := ValueToProto(ev.Payload)
	if err != nil {
		return nil, fmt.Errorf("event payload: %w", err)
	}
	p := &pb.Event{Op: ev.Op, Channel: ev.Channel, Key: key, Payload: payload}
	if !ev.Time.IsZero() {
		p.Time = TimeToProto(ev.Time)
	}
	if ev.Err != nil {
		p.Error = ErrorInfoFromError(ev.Err, redact)
	}
	return p, nil
}

// EventFromProto decodes a change event; a nil event is ErrInvalid, an unset
// payload is NULL and an error becomes a *driver.Error (see FromErrorInfo).
func EventFromProto(p *pb.Event) (driver.Event, error) {
	if p == nil {
		return driver.Event{}, invalidf("missing event")
	}
	if err := checkNames("event name", []string{p.GetOp(), p.GetChannel()}); err != nil {
		return driver.Event{}, err
	}
	ev := driver.Event{Op: p.GetOp(), Channel: p.GetChannel(), Payload: driver.Null()}
	var err error
	if ev.Time, err = TimeFromProto(p.GetTime()); err != nil {
		return driver.Event{}, fmt.Errorf("event time: %w", err)
	}
	if ev.Key, err = ObjectFromProto(p.GetKey()); err != nil {
		return driver.Event{}, fmt.Errorf("event key: %w", err)
	}
	if p.GetPayload() != nil {
		if ev.Payload, err = ValueFromProto(p.GetPayload()); err != nil {
			return driver.Event{}, fmt.Errorf("event payload: %w", err)
		}
	}
	if p.GetError() != nil {
		ev.Err = FromErrorInfo(p.GetError(), nil)
	}
	return ev, nil
}
