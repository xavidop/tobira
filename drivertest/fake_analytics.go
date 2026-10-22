package drivertest

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

var (
	_ driver.Watcher          = (*conn)(nil)
	_ driver.VectorSearcher   = (*conn)(nil)
	_ driver.TimeSeriesReader = (*conn)(nil)
	_ driver.GraphReader      = (*conn)(nil)
)

// ---- Watcher ----

type subscription struct {
	ch    chan driver.Event
	ns    string // "" = any namespace
	name  string
	owner *conn // the Conn whose Close ends it
}

// endSubLocked ends sub: it leaves f.subs and its channel closes. Ending
// an ended sub does nothing. f.mu must be held.
func (f *Fake) endSubLocked(sub *subscription) {
	if _, ok := f.subs[sub]; !ok {
		return
	}
	delete(f.subs, sub)
	close(sub.ch)
}

const watchBuffer = 256

// emit publishes a Mutator event. Inside a Tx the event is held until Commit.
// f.mu must be held.
func (s *session) emit(ref driver.CollectionRef, op string, key driver.Object, payload driver.Value) {
	ev := driver.Event{Time: time.Now(), Op: op, Channel: ref.Name, Key: key, Payload: payload}
	if s.log != nil {
		return // a Tx emits its events at Commit
	}
	s.f.emitLocked(ref, ev)
}

// emitLocked delivers ev to matching subscribers, dropping it for a subscriber
// whose buffer is full. f.mu must be held.
func (f *Fake) emitLocked(ref driver.CollectionRef, ev driver.Event) {
	for sub := range f.subs {
		if sub.name == ref.Name && (sub.ns == "" || sub.ns == ref.Namespace) {
			select {
			case sub.ch <- ev:
			default:
			}
		}
	}
}

// Emit delivers ev to every watcher of ref (a watcher of a channel matches
// ref.Name), as if a write had happened. A watcher whose buffer is full
// misses it, as it would miss a write. Tests use it to script a stream: an
// event of any Op, or one with Err set.
func (f *Fake) Emit(ref driver.CollectionRef, ev driver.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emitLocked(ref, ev)
}

// Watchers is how many watches are open: a watch counts from Watch until
// its ctx ends or its Conn closes. Close ends its Conn's watches before it
// returns, so Watchers is exact right after Close.
func (f *Fake) Watchers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

// Watch emits one Event per Mutator call (committed Tx writes are emitted at
// Commit) on the collection, or on the collection named by Channel. The
// channel closes when ctx ends or the Conn is closed.
func (c *conn) Watch(ctx context.Context, req driver.WatchRequest) (_ <-chan driver.Event, err error) {
	if err := c.need("watch", driver.CapWatcher); err != nil {
		return nil, err
	}
	var ref driver.CollectionRef
	if req.Collection != nil {
		ref = *req.Collection
	}
	idx, err := c.enter(ctx, "watch", ref, driver.Query{})
	defer c.f.finish(idx, &err)
	if err != nil {
		return nil, err
	}
	sub := &subscription{ch: make(chan driver.Event, watchBuffer), ns: ref.Namespace, name: ref.Name, owner: c}
	switch {
	case req.Collection != nil:
	case req.Channel != "":
		sub.name = req.Channel
	default:
		return nil, fakeErr(driver.ErrSyntax, "watch", "need a collection or a channel")
	}
	c.f.mu.Lock()
	if c.isClosed.Load() {
		c.f.mu.Unlock()
		return nil, fakeErr(driver.ErrConnection, "watch", "connection is closed")
	}
	c.f.subs[sub] = struct{}{}
	c.f.mu.Unlock()
	// Close ends the watch itself, before it returns; this ends it when
	// ctx does.
	go func() {
		select {
		case <-ctx.Done():
		case <-c.done:
		}
		c.f.mu.Lock()
		c.f.endSubLocked(sub)
		c.f.mu.Unlock()
	}()
	return sub.ch, nil
}

// ---- VectorSearcher ----

// vectorDistance returns the distance between a and b under m, ranked
// nearest first: Euclidean for l2, 1 - cosine similarity for cosine (the
// default) and the negated dot product for ip.
func vectorDistance(m driver.VectorMetric, a, b []float32) float64 {
	var dot, na, nb, sq float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
		sq += (x - y) * (x - y)
	}
	switch m {
	case driver.MetricL2:
		return math.Sqrt(sq)
	case driver.MetricInnerProduct:
		return -dot
	}
	if na == 0 || nb == 0 {
		return 1
	}
	return 1 - dot/(math.Sqrt(na)*math.Sqrt(nb))
}

// Similar ranks rows by the exact distance under q.Metric (l2, cosine or
// ip; "" is cosine) to q.Vector over the first Vector column (or q.Field)
// and appends a "_distance" column. K defaults to 10. A Text query is
// embedded with ConnConfig.Embedder; without one it is ErrUnsupported
// (driver.NeedEmbedderHint), and an embedding whose size differs from the
// vectors is ErrSyntax.
func (c *conn) Similar(ctx context.Context, ref driver.CollectionRef, q driver.VectorQuery) (_ driver.ResultSet, err error) {
	if err := c.need("similar", driver.CapVectorSearcher); err != nil {
		return nil, err
	}
	idx, err := c.enter(ctx, "similar", ref, driver.Query{})
	defer c.f.finish(idx, &err)
	if err != nil {
		return nil, err
	}
	switch q.Metric {
	case driver.MetricDefault, driver.MetricL2, driver.MetricCosine, driver.MetricInnerProduct:
	default:
		return nil, fakeErr(driver.ErrUnsupported, "similar", "metric %q is not supported", q.Metric)
	}
	if len(q.Vector) == 0 && q.Text != "" {
		if c.embedder == nil {
			return nil, fakeErr(driver.ErrUnsupported, "similar", "%s", driver.NeedEmbedderHint)
		}
		vecs, err := c.embedder.Embed(ctx, []string{q.Text})
		if err != nil {
			return nil, driver.Wrap(driver.ErrUnsupported, fakeName, "similar", err)
		}
		if len(vecs) != 1 {
			return nil, fakeErr(driver.ErrUnsupported, "similar", "the embedder returned %d vectors for 1 text", len(vecs))
		}
		q.Vector = vecs[0]
	}
	start := time.Now()
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	ref, t, err := c.tables().resolve("similar", ref)
	if err != nil {
		return nil, err
	}
	if len(q.Vector) == 0 {
		return nil, fakeErr(driver.ErrSyntax, "similar", "empty vector")
	}
	var vi int
	if q.Field != "" {
		if vi = t.colIndex(q.Field); vi < 0 {
			return nil, fakeErr(driver.ErrNotFound, "similar", "no such field %q", q.Field)
		}
		if t.cols[vi].Type != driver.TypeVector {
			return nil, fakeErr(driver.ErrUnsupported, "similar", "field %q is not a vector", q.Field)
		}
	} else if vi = slices.IndexFunc(t.cols, func(col driver.Column) bool { return col.Type == driver.TypeVector }); vi < 0 {
		return nil, fakeErr(driver.ErrUnsupported, "similar", "%s has no vector column", ref.Name)
	}
	rows, err := filterRows("similar", t, q.Filter, t.rows)
	if err != nil {
		return nil, err
	}
	type scored struct {
		row  driver.Row
		dist float64
	}
	var hits []scored
	dims := -1
	for _, r := range rows {
		if v, ok := r[vi].AsVector(); ok {
			dims = len(v)
			if len(v) == len(q.Vector) {
				hits = append(hits, scored{r, vectorDistance(q.Metric, q.Vector, v)})
			}
		}
	}
	if len(hits) == 0 && dims >= 0 {
		return nil, fakeErr(driver.ErrSyntax, "similar", "query vector has %d dimensions, the vectors have %d", len(q.Vector), dims)
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].dist < hits[b].dist })
	k := q.K
	if k <= 0 {
		k = 10
	}
	truncated := len(hits) > k
	hits = hits[:min(k, len(hits))]
	out := make([]driver.Row, len(hits))
	for i, h := range hits {
		out[i] = append(slices.Clone(h.row), driver.Float(h.dist))
	}
	cols := append(t.columns(ref), driver.Column{Name: "_distance", Type: driver.TypeFloat})
	return driver.NewSliceResultSet(cols, out, driver.Stats{RowsAffected: -1, Truncated: truncated, Duration: time.Since(start)}), nil
}

// ---- TimeSeriesReader ----

type aggregator struct {
	bucket time.Time
	groups []driver.Value
	vals   []float64
	last   float64
}

func (a *aggregator) result(agg string) driver.Value {
	if agg == "count" {
		return driver.Int(int64(len(a.vals)))
	}
	switch agg {
	case "last":
		return driver.Float(a.last)
	case "min":
		return driver.Float(slices.Min(a.vals))
	case "max":
		return driver.Float(slices.Max(a.vals))
	}
	sum := 0.0
	for _, v := range a.vals {
		sum += v
	}
	if agg == "mean" {
		sum /= float64(len(a.vals))
	}
	return driver.Float(sum)
}

// epochWindow returns the start of the window of width w holding ts,
// aligned to the Unix epoch in UTC.
func epochWindow(ts time.Time, w time.Duration) time.Time {
	n := ts.UnixNano()
	r := n % int64(w)
	if r < 0 {
		r += int64(w)
	}
	return time.Unix(0, n-r).UTC()
}

// Range reads the first Time column of the collection. q.Field names the value
// column to aggregate ("" = the first non-key numeric column). With
// Window == 0 the raw points in [Start, End) come back; otherwise points are
// bucketed by Window (aligned to the Unix epoch, UTC) and aggregated
// (default mean) per group. Columns: _time (the window start), the GroupBy
// fields, value. NULL values are skipped.
func (c *conn) Range(ctx context.Context, ref driver.CollectionRef, q driver.RangeQuery) (_ driver.ResultSet, err error) {
	if err := c.need("range", driver.CapTimeSeriesReader); err != nil {
		return nil, err
	}
	idx, err := c.enter(ctx, "range", ref, driver.Query{})
	defer c.f.finish(idx, &err)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	ref, t, err := c.tables().resolve("range", ref)
	if err != nil {
		return nil, err
	}
	ti := slices.IndexFunc(t.cols, func(col driver.Column) bool { return col.Type == driver.TypeTime })
	if ti < 0 {
		return nil, fakeErr(driver.ErrUnsupported, "range", "%s has no time column", ref.Name)
	}
	var vi int
	if q.Field != "" {
		if vi = t.colIndex(q.Field); vi < 0 {
			return nil, fakeErr(driver.ErrNotFound, "range", "no such field %q", q.Field)
		}
	} else if vi = slices.IndexFunc(t.cols, func(col driver.Column) bool {
		return !col.Key && (col.Type == driver.TypeInt || col.Type == driver.TypeFloat || col.Type == driver.TypeDecimal)
	}); vi < 0 {
		return nil, fakeErr(driver.ErrUnsupported, "range", "%s has no numeric column", ref.Name)
	}
	agg := q.Aggregate
	if agg == "" {
		agg = "mean"
	}
	if !slices.Contains([]string{"mean", "sum", "min", "max", "count", "last"}, agg) {
		return nil, fakeErr(driver.ErrUnsupported, "range", "aggregate %q is not supported", agg)
	}
	gis := make([]int, len(q.GroupBy))
	for i, g := range q.GroupBy {
		if gis[i] = t.colIndex(g); gis[i] < 0 {
			return nil, fakeErr(driver.ErrNotFound, "range", "no such field %q", g)
		}
	}
	rows, err := filterRows("range", t, q.Filter, t.rows)
	if err != nil {
		return nil, err
	}
	var pts []driver.Row
	for _, r := range rows {
		ts, ok := r[ti].AsTime()
		if !ok || (!q.Start.IsZero() && ts.Before(q.Start)) || (!q.End.IsZero() && !ts.Before(q.End)) {
			continue
		}
		pts = append(pts, r)
	}
	sort.SliceStable(pts, func(a, b int) bool { return compareValues(pts[a][ti], pts[b][ti]) < 0 })

	cols := []driver.Column{{Name: "_time", Type: driver.TypeTime}}
	for _, gi := range gis {
		cols = append(cols, t.cols[gi])
	}
	valCol := driver.Column{Name: "value", Type: driver.TypeFloat}
	var out []driver.Row
	if q.Window <= 0 {
		valCol.Type = t.cols[vi].Type
		for _, r := range pts {
			if r[vi].IsNull() {
				continue
			}
			row := driver.Row{r[ti]}
			for _, gi := range gis {
				row = append(row, r[gi])
			}
			out = append(out, append(row, r[vi]))
		}
	} else {
		if agg == "count" {
			valCol.Type = driver.TypeInt
		}
		var buckets []*aggregator
		index := map[string]*aggregator{}
		for _, r := range pts {
			ts, _ := r[ti].AsTime()
			f, ok := numeric(r[vi])
			if !ok {
				continue
			}
			x, _ := f.Float64()
			bucket := epochWindow(ts, q.Window)
			var kb strings.Builder
			kb.WriteString(bucket.Format(time.RFC3339Nano))
			groups := make([]driver.Value, len(gis))
			for i, gi := range gis {
				groups[i] = r[gi]
				kb.WriteByte(0)
				kb.WriteString(r[gi].String())
			}
			a := index[kb.String()]
			if a == nil {
				a = &aggregator{bucket: bucket, groups: groups}
				index[kb.String()] = a
				buckets = append(buckets, a)
			}
			a.vals = append(a.vals, x)
			a.last = x
		}
		sort.SliceStable(buckets, func(i, j int) bool { return buckets[i].bucket.Before(buckets[j].bucket) })
		for _, a := range buckets {
			out = append(out, append(append(driver.Row{driver.Time(a.bucket)}, a.groups...), a.result(agg)))
		}
	}
	cols = append(cols, valCol)
	return driver.NewSliceResultSet(cols, out, driver.Stats{RowsAffected: -1, Duration: time.Since(start)}), nil
}

// ---- GraphReader ----

type graphEdge struct {
	edge     driver.Edge
	from, to string
}

// graphLabels reads a node's labels from a "label" string or a "labels" list/JSON column.
func graphLabels(t *table, r driver.Row) []string {
	if i := t.colIndex("labels"); i >= 0 {
		if items, ok := r[i].AsList(); ok {
			var out []string
			for _, it := range items {
				out = append(out, idText(it))
			}
			return out
		}
		if raw, ok := r[i].AsJSON(); ok {
			var out []string
			if json.Unmarshal(raw, &out) == nil {
				return out
			}
		}
	}
	if i := t.colIndex("label"); i >= 0 && !r[i].IsNull() {
		return []string{idText(r[i])}
	}
	return nil
}

func graphProps(t *table, r driver.Row, skip ...string) driver.Object {
	var props driver.Object
	for i, col := range t.cols {
		if !slices.Contains(skip, col.Name) {
			props = append(props, driver.Member{Key: col.Name, Value: r[i]})
		}
	}
	return props
}

// Neighbors treats the collections named "nodes" (columns id, label|labels,
// props…) and "edges" (id, from, to, type, props…) as a graph and walks it
// breadth-first from q.Start, ignoring edge direction, to q.Depth (default 1)
// and at most q.Limit nodes (default 100). Labels, when set, restricts the
// nodes reached. Edges between reached nodes are returned.
func (c *conn) Neighbors(ctx context.Context, q driver.GraphQuery) (_ *driver.Graph, err error) {
	if err := c.need("neighbors", driver.CapGraphReader); err != nil {
		return nil, err
	}
	idx, err := c.enter(ctx, "neighbors", driver.CollectionRef{}, driver.Query{})
	defer c.f.finish(idx, &err)
	if err != nil {
		return nil, err
	}
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	d := c.tables()
	_, nt, ok1 := d.findByName("nodes")
	_, et, ok2 := d.findByName("edges")
	if !ok1 || !ok2 {
		return nil, fakeErr(driver.ErrNotFound, "neighbors", "no nodes/edges collections")
	}
	nid, ef, eto := nt.colIndex("id"), et.colIndex("from"), et.colIndex("to")
	if nid < 0 || ef < 0 || eto < 0 {
		return nil, fakeErr(driver.ErrUnsupported, "neighbors", "nodes need id; edges need from and to")
	}
	nodes := map[string]driver.Node{}
	for _, r := range nt.rows {
		id := idText(r[nid])
		nodes[id] = driver.Node{ID: id, Labels: graphLabels(nt, r), Props: graphProps(nt, r, "id", "label", "labels")}
	}
	var edges []graphEdge
	for i, r := range et.rows {
		e := graphEdge{from: idText(r[ef]), to: idText(r[eto])}
		e.edge = driver.Edge{ID: e.from + "->" + e.to + "#" + itoa(i), From: e.from, To: e.to, Props: graphProps(et, r, "id", "from", "to", "type")}
		if ii := et.colIndex("id"); ii >= 0 {
			e.edge.ID = idText(r[ii])
		}
		if ti := et.colIndex("type"); ti >= 0 {
			e.edge.Type = idText(r[ti])
		}
		edges = append(edges, e)
	}
	start := idText(q.Start)
	if _, ok := nodes[start]; !ok {
		return nil, fakeErr(driver.ErrNotFound, "neighbors", "node %q not found", start)
	}
	depth, limit := q.Depth, q.Limit
	if depth <= 0 {
		depth = 1
	}
	if limit <= 0 {
		limit = 100
	}
	visited := map[string]bool{start: true}
	order, frontier := []string{start}, []string{start}
	for d := 0; d < depth && len(frontier) > 0; d++ {
		var next []string
		for _, cur := range frontier {
			for _, e := range edges {
				other := ""
				switch cur {
				case e.from:
					other = e.to
				case e.to:
					other = e.from
				default:
					continue
				}
				n, ok := nodes[other]
				if !ok || visited[other] || len(order) >= limit {
					continue
				}
				if len(q.Labels) > 0 && !slices.ContainsFunc(n.Labels, func(l string) bool { return slices.Contains(q.Labels, l) }) {
					continue
				}
				visited[other] = true
				order = append(order, other)
				next = append(next, other)
			}
		}
		frontier = next
	}
	g := &driver.Graph{}
	for _, id := range order {
		g.Nodes = append(g.Nodes, nodes[id])
	}
	for _, e := range edges {
		if visited[e.from] && visited[e.to] {
			g.Edges = append(g.Edges, e.edge)
		}
	}
	return g, nil
}
