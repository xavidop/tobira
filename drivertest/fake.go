package drivertest

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xavidop/tobira/driver"
)

const fakeName = "fake"

// allCapabilities lists every capability the Fake can offer, in the order of
// the driver.Cap constants, then the declared CapReadOnly (the Fake enforces
// Query.ReadOnly whether or not it is declared).
var allCapabilities = []driver.Capability{
	driver.CapBrowser, driver.CapMutator, driver.CapExplainer, driver.CapTransactor,
	driver.CapVectorSearcher, driver.CapTimeSeriesReader, driver.CapGraphReader,
	driver.CapCompleter, driver.CapExporter, driver.CapWatcher,
	driver.CapReadOnly,
}

// Call is one recorded driver call. Err is the error the call returned.
type Call struct {
	Op    string
	Ref   driver.CollectionRef
	Query driver.Query
	Err   error
}

type fakeOptions struct {
	disabled                      map[driver.Capability]bool
	people                        bool
	docs, kv, vectors, timeSeries bool
	wide, search                  bool
	kind                          driver.Kind
}

// FakeOption configures NewFake.
type FakeOption func(*fakeOptions)

// WithPeople seeds the People dataset into namespace "main" as collection
// "tobira_people".
func WithPeople() FakeOption { return func(o *fakeOptions) { o.people = true } }

// WithDocuments seeds the document fixture (Documents) into namespace
// "main" as the schemaless collection DocumentsCollection.
func WithDocuments() FakeOption { return func(o *fakeOptions) { o.docs = true } }

// WithKeyValues seeds the key-value fixture (KeyValueDataset) into namespace
// "main" as collection KeyValueCollection, listed as a key pattern.
func WithKeyValues() FakeOption { return func(o *fakeOptions) { o.kv = true } }

// WithVectors seeds the vector fixture (VectorDataset) into namespace "main"
// as collection VectorCollection.
func WithVectors() FakeOption { return func(o *fakeOptions) { o.vectors = true } }

// WithTimeSeries seeds the time-series fixture (TimeSeriesDataset) into
// namespace "main" as measurement TimeSeriesCollection.
func WithTimeSeries() FakeOption { return func(o *fakeOptions) { o.timeSeries = true } }

// WithWideColumns seeds the wide-column fixture (WideColumnDataset) into
// namespace "main" as table WideColumnCollection: Describe reports its
// partition and clustering keys, and its rows are kept in partition then
// clustering order, as a wide-column store returns them.
func WithWideColumns() FakeOption { return func(o *fakeOptions) { o.wide = true } }

// WithSearch seeds the search fixture (SearchDocuments) into namespace
// "main" as the schemaless index SearchCollection, which the query
// language's search statement reads.
func WithSearch() FakeOption { return func(o *fakeOptions) { o.search = true } }

// WithKind makes Info report kind instead of KindOther, to test code
// that picks views or checks by kind.
func WithKind(kind driver.Kind) FakeOption { return func(o *fakeOptions) { o.kind = kind } }

// WithoutCapabilities removes capabilities from the Fake: Info no longer lists
// them, Supports reports false (so driver.As fails) and calling the method
// directly returns ErrUnsupported.
func WithoutCapabilities(caps ...driver.Capability) FakeOption {
	return func(o *fakeOptions) {
		for _, c := range caps {
			o.disabled[c] = true
		}
	}
}

type queryHandler struct {
	exact string
	re    *regexp.Regexp
	fn    func(driver.Query) (driver.ResultSet, error)
}

// Fake is an in-memory, scriptable driver implementing every capability. It is
// used by TUI/CLI/MCP tests and passes Run itself. All Conns opened from one
// Fake share the same data, and a Fake is safe for concurrent use.
//
// Every error it returns is a *driver.Error with Driver "fake". Ops, as
// recorded by Calls and matched by FailNext: open, ping, namespaces,
// collections, describe, query, browse, insert, update, delete, begin, commit,
// rollback, explain, complete, export, watch, similar, range, neighbors.
//
// The query language is described on the unexported parser; in short:
//
//	select * from <coll> [where <field> = <literal>] [order by <field> [asc|desc]] [limit N]
//	search <coll> for '<term>'  full-text hits with a _score column, best first
//	insert|update|delete ...   a write: RowsAffected=1 (only "delete from <c> where <f> = <lit>" deletes)
//	sleep <duration>           blocks, honouring ctx; no columns, RowsAffected=0 (like DDL)
//
// Statements may be separated by ';' (the result then implements
// driver.MultiResultSet). They run in order and each statement's effects apply
// immediately, even if a later statement fails. Sorting treats NULL as greater than every value:
// nulls sort last ascending and first descending.
type Fake struct {
	disabled map[driver.Capability]bool // immutable after NewFake
	kind     driver.Kind                // immutable after NewFake

	mu       sync.Mutex
	data     *tables
	handlers []queryHandler
	failures map[string][]error
	describe map[driver.CollectionRef]func(*driver.CollectionSchema)
	latency  time.Duration
	closeLat time.Duration
	calls    []Call
	subs     map[*subscription]struct{}
}

// NewFake returns an empty Fake (Info().Name "fake", scheme "fake").
func NewFake(opts ...FakeOption) *Fake {
	o := fakeOptions{disabled: map[driver.Capability]bool{}}
	for _, opt := range opts {
		opt(&o)
	}
	f := &Fake{
		disabled: o.disabled,
		kind:     cmp.Or(o.kind, driver.KindOther),
		data:     newTables(),
		failures: map[string][]error{},
		describe: map[driver.CollectionRef]func(*driver.CollectionSchema){},
		subs:     map[*subscription]struct{}{},
	}
	if o.people {
		p := PeopleDataset()
		f.AddCollection("main", peopleCollection, p.Columns, p.Rows, p.Key...)
	}
	if o.docs {
		f.AddDocuments("main", DocumentsCollection, Documents())
	}
	if o.wide {
		f.addWideColumns("main", WideColumnCollection, WideColumnDataset(), WideColumnPartitionKey(), WideColumnClusteringKey())
	}
	if o.search {
		f.AddDocuments("main", SearchCollection, SearchDocuments())
		f.setType("main", SearchCollection, driver.CollectionIndex)
	}
	for _, k := range []struct {
		on    bool
		name  string
		data  Dataset
		ctype driver.CollectionType
	}{
		{o.kv, KeyValueCollection, KeyValueDataset(), driver.CollectionKeyPattern},
		{o.vectors, VectorCollection, VectorDataset(), driver.CollectionCollection},
		{o.timeSeries, TimeSeriesCollection, TimeSeriesDataset(), driver.CollectionMeasurement},
	} {
		if k.on {
			f.AddCollection("main", k.name, k.data.Columns, k.data.Rows, k.data.Key...)
			f.setType("main", k.name, k.ctype)
		}
	}
	return f
}

// setType sets the collection type a collection is listed with.
func (f *Fake) setType(ns, name string, ct driver.CollectionType) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t := f.data.m[driver.CollectionRef{Namespace: ns, Name: name}]; t != nil {
		t.ctype = ct
	}
}

// AddDocuments adds or replaces a schemaless document collection keyed by
// _id. Its columns are the union of the documents' top-level fields in
// first-seen order; a field a document lacks reads as NULL, and a result
// lists only the fields that hold a value in one of its rows, so pages can
// differ in columns. Describe reports Schemaless with the inferred fields
// (a type union such as "int|string" for mixed values, nested Fields for
// Objects) and a Sample. Inputs are copied.
func (f *Fake) AddDocuments(ns, name string, docs []driver.Object) {
	var cols []driver.Column
	index := map[string]int{}
	for _, d := range docs {
		for _, m := range d {
			if _, ok := index[m.Key]; !ok {
				index[m.Key] = len(cols)
				cols = append(cols, driver.Column{Name: m.Key})
			}
		}
	}
	rows := make([]driver.Row, len(docs))
	for i, d := range docs {
		r := make(driver.Row, len(cols))
		for _, m := range d {
			r[index[m.Key]] = m.Value
		}
		rows[i] = r
	}
	for i := range cols {
		cols[i].Type, cols[i].NativeType = inferType(rows, i)
	}
	var key []string
	if _, ok := index["_id"]; ok {
		key = []string{"_id"}
	}
	f.AddCollection(ns, name, cols, rows, key...)
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.data.m[driver.CollectionRef{Namespace: ns, Name: name}]
	t.ctype, t.schemaless = driver.CollectionCollection, true
}

// addWideColumns adds a wide-column table: d keyed by its partition key
// then its clustering columns, with rows sorted by partition (ascending,
// standing in for token order) then clustering order.
func (f *Fake) addWideColumns(ns, name string, d Dataset, partition []string, clustering []driver.SortKey) {
	keys := make([]driver.SortKey, 0, len(partition)+len(clustering))
	for _, p := range partition {
		keys = append(keys, driver.SortKey{Field: p})
	}
	keys = append(keys, clustering...)
	sortByKeys(d.Columns, d.Rows, keys)
	f.AddCollection(ns, name, d.Columns, d.Rows, d.Key...)
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.data.m[driver.CollectionRef{Namespace: ns, Name: name}]
	t.partition, t.clustering = slices.Clone(partition), slices.Clone(clustering)
}

// Driver returns the driver.Driver face of the Fake.
func (f *Fake) Driver() driver.Driver { return fakeDriver{f} }

// AddCollection adds or replaces a collection. Inputs are copied. It panics
// if a row's width differs from len(cols), which is a test-programming error.
func (f *Fake) AddCollection(ns, name string, cols []driver.Column, rows []driver.Row, key ...string) {
	t := &table{cols: slices.Clone(cols), key: slices.Clone(key)}
	for i, r := range rows {
		if len(r) != len(cols) {
			panic(fmt.Sprintf("drivertest: AddCollection %s.%s: row %d has %d values for %d columns", ns, name, i, len(r), len(cols)))
		}
	}
	for i := range t.cols {
		t.cols[i].Key = slices.Contains(key, t.cols[i].Name)
	}
	for _, r := range rows {
		t.rows = append(t.rows, slices.Clone(r))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data.put(driver.CollectionRef{Namespace: ns, Name: name}, t)
}

// OnQuery scripts the result of a query. match is the exact query text, or
// "re:<regexp>" for a regular expression (an invalid one panics). Handlers
// are tried in registration order and take precedence over the built-in query
// language; the handler's error is returned as-is.
func (f *Fake) OnQuery(match string, fn func(driver.Query) (driver.ResultSet, error)) {
	h := queryHandler{exact: match, fn: fn}
	if rest, ok := strings.CutPrefix(match, "re:"); ok {
		h.re = regexp.MustCompile(rest)
	}
	f.mu.Lock()
	f.handlers = append(f.handlers, h)
	f.mu.Unlock()
}

// OnDescribe registers fn to amend what Describe reports for ns.name, after
// the Fake has built the schema from the collection: foreign keys, comments,
// defaults, DDL and the like, which the Fake cannot infer. fn runs with the
// Fake's lock held, so it must not call the Fake. A later call replaces fn.
func (f *Fake) OnDescribe(ns, name string, fn func(*driver.CollectionSchema)) {
	f.mu.Lock()
	f.describe[driver.CollectionRef{Namespace: ns, Name: name}] = fn
	f.mu.Unlock()
}

// FailNext queues err to be returned, as-is, by the next call of op. Errors
// queue per op in FIFO order. A nil err is ignored.
func (f *Fake) FailNext(op string, err error) {
	if err == nil {
		return
	}
	f.mu.Lock()
	f.failures[op] = append(f.failures[op], err)
	f.mu.Unlock()
}

// SetLatency delays every call by d. The delay honours context cancellation.
func (f *Fake) SetLatency(d time.Duration) {
	f.mu.Lock()
	f.latency = d
	f.mu.Unlock()
}

// SetCloseLatency makes every Conn.Close block for d before it closes, to
// test callers that must not wait on a slow close.
func (f *Fake) SetCloseLatency(d time.Duration) {
	f.mu.Lock()
	f.closeLat = d
	f.mu.Unlock()
}

// Calls returns a copy of the recorded calls, oldest first.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *Fake) enabled() []driver.Capability {
	var out []driver.Capability
	for _, c := range allCapabilities {
		if !f.disabled[c] {
			out = append(out, c)
		}
	}
	return out
}

func (f *Fake) finish(idx int, err *error) {
	f.mu.Lock()
	f.calls[idx].Err = *err
	f.mu.Unlock()
}

// fakeErr builds a *driver.Error attributed to the Fake.
func fakeErr(kind error, op, format string, args ...any) error {
	return driver.Wrap(kind, fakeName, op, fmt.Errorf(format, args...))
}

func ctxErr(op string, err error) error { return driver.Wrap(nil, fakeName, op, err) }

// session is the state shared by a Conn and its Txs: which data they see and
// whether writes are refused.
type session struct {
	f      *Fake
	ro     bool
	data   **tables        // the dataset this session reads and writes (guarded by f.mu)
	closed func() string   // "" while usable, else why not (closed Conn, finished Tx)
	log    *[]txOp         // non-nil in a Tx: mutations are logged and applied at Commit
	done   <-chan struct{} // closed with the Conn; ends Watch goroutines
}

type pendingEvt struct {
	ref driver.CollectionRef
	ev  driver.Event
}

// enter records the call, then applies the closed check, cancellation,
// latency and any FailNext error, in that order.
func (s *session) enter(ctx context.Context, op string, ref driver.CollectionRef, q driver.Query) (int, error) {
	f := s.f
	f.mu.Lock()
	idx := len(f.calls)
	f.calls = append(f.calls, Call{Op: op, Ref: ref, Query: q})
	lat := f.latency
	f.mu.Unlock()
	if why := s.closed(); why != "" {
		return idx, fakeErr(driver.ErrConnection, op, "%s", why)
	}
	if err := ctx.Err(); err != nil {
		return idx, ctxErr(op, err)
	}
	if lat > 0 {
		timer := time.NewTimer(lat)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return idx, ctxErr(op, ctx.Err())
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if q := f.failures[op]; len(q) > 0 {
		err := q[0]
		f.failures[op] = q[1:]
		return idx, err
	}
	return idx, nil
}

// need returns ErrUnsupported when capability c has been removed.
func (s *session) need(op string, c driver.Capability) error {
	if s.f.disabled[c] {
		return fakeErr(driver.ErrUnsupported, op, "capability %s is disabled", c)
	}
	return nil
}

// tables returns the session's dataset. f.mu must be held.
func (s *session) tables() *tables { return *s.data }

type fakeDriver struct{ f *Fake }

func (d fakeDriver) Info() driver.Info {
	return driver.Info{
		Name:         fakeName,
		Kind:         d.f.kind,
		Schemes:      []string{"fake"},
		Dialect:      "fake",
		Version:      "0.1.0",
		Capabilities: d.f.enabled(),
	}
}

func (d fakeDriver) Open(ctx context.Context, cfg driver.ConnConfig) (c driver.Conn, err error) {
	s := &session{f: d.f, closed: func() string { return "" }}
	idx, err := s.enter(ctx, "open", driver.CollectionRef{}, driver.Query{})
	defer d.f.finish(idx, &err)
	if err != nil {
		return nil, err
	}
	conn := &conn{done: make(chan struct{}), embedder: cfg.Embedder}
	conn.session = session{f: d.f, ro: cfg.ReadOnly, data: &d.f.data, done: conn.done, closed: func() string {
		if conn.isClosed.Load() {
			return "connection is closed"
		}
		return ""
	}}
	return conn, nil
}

// conn is the Fake's driver.Conn. It implements every capability interface
// statically and reports the enabled ones through Supports.
type conn struct {
	session
	embedder driver.Embedder // ConnConfig.Embedder; embeds text for Similar
	isClosed atomic.Bool
	once     sync.Once
	done     chan struct{} // closed by Close
}

// Supports implements driver.Supporter.
func (c *conn) Supports(cp driver.Capability) bool { return !c.f.disabled[cp] }

func (c *conn) Close() error {
	c.f.mu.Lock()
	lat := c.f.closeLat
	c.f.mu.Unlock()
	if lat > 0 {
		time.Sleep(lat)
	}
	c.once.Do(func() {
		c.f.mu.Lock()
		c.isClosed.Store(true)
		for sub := range c.f.subs {
			if sub.owner == c {
				c.f.endSubLocked(sub)
			}
		}
		c.f.mu.Unlock()
		close(c.done)
	})
	return nil
}

func (c *conn) Ping(ctx context.Context) (err error) {
	idx, err := c.enter(ctx, "ping", driver.CollectionRef{}, driver.Query{})
	defer c.f.finish(idx, &err)
	return err
}

func (c *conn) Namespaces(ctx context.Context) (_ []driver.Namespace, err error) {
	idx, err := c.enter(ctx, "namespaces", driver.CollectionRef{}, driver.Query{})
	defer c.f.finish(idx, &err)
	if err != nil {
		return nil, err
	}
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	d := c.tables()
	def := d.defaultNamespace()
	var out []driver.Namespace
	for _, ns := range d.namespaces() {
		out = append(out, driver.Namespace{Name: ns, Label: "namespace", Default: ns == def})
	}
	return out, nil
}

func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) (_ []driver.Collection, err error) {
	idx, err := c.enter(ctx, "collections", driver.CollectionRef{Namespace: ns.Name}, driver.Query{})
	defer c.f.finish(idx, &err)
	if err != nil {
		return nil, err
	}
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	d := c.tables()
	name := ns.Name
	if name == "" {
		name = d.defaultNamespace()
	}
	if !slices.Contains(d.namespaces(), name) {
		return nil, fakeErr(driver.ErrNotFound, "collections", "namespace %q not found", name)
	}
	var out []driver.Collection
	for _, ref := range d.order {
		if ref.Namespace == name {
			out = append(out, driver.Collection{Ref: ref, Type: d.m[ref].collectionType(), RowEstimate: int64(len(d.m[ref].rows))})
		}
	}
	return out, nil
}

func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (_ *driver.CollectionSchema, err error) {
	idx, err := c.enter(ctx, "describe", ref, driver.Query{})
	defer c.f.finish(idx, &err)
	if err != nil {
		return nil, err
	}
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	ref, t, err := c.tables().resolve("describe", ref)
	if err != nil {
		return nil, err
	}
	sch := &driver.CollectionSchema{
		Ref: ref, Type: t.collectionType(), PrimaryKey: slices.Clone(t.key),
		PartitionKey: slices.Clone(t.partition), ClusteringKey: slices.Clone(t.clustering),
		RowEstimate: int64(len(t.rows)), SizeBytes: -1, Schemaless: t.schemaless,
	}
	for i, col := range t.cols {
		native := col.NativeType
		if native == "" {
			native = col.Type.String()
		}
		// A missing single integer key is assigned max+1 (doInsert).
		auto := col.Key && len(t.key) == 1 && col.Type == driver.TypeInt
		fd := driver.Field{Name: col.Name, NativeType: native, Type: col.Type, Nullable: !col.Key, AutoIncrement: auto}
		if t.schemaless {
			fd.Fields = nestedFields(t.rows, i)
		}
		sch.Fields = append(sch.Fields, fd)
	}
	if t.schemaless {
		sch.Sample = cloneRows(t.rows[:min(len(t.rows), sampleSize)])
	}
	if len(t.key) > 0 {
		sch.Indexes = []driver.Index{{Name: ref.Name + "_pkey", Fields: slices.Clone(t.key), Unique: true, Primary: true, Method: "fake"}}
	}
	if fn := c.f.describe[ref]; fn != nil {
		fn(sch)
	}
	return sch, nil
}

func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	return c.query(ctx, q)
}

var (
	_ driver.Conn      = (*conn)(nil)
	_ driver.Supporter = (*conn)(nil)
)
