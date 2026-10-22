package driver

import (
	"context"
	"io"
	"reflect"
	"slices"
	"time"
)

// Capability names an optional interface a Conn (or Tx) may implement, or a
// declared guarantee such as CapReadOnly.
type Capability string

// The interface capabilities, in the order CapabilitiesOf reports them.
const (
	CapBrowser          Capability = "browser"
	CapMutator          Capability = "mutator"
	CapExplainer        Capability = "explainer"
	CapTransactor       Capability = "transactor"
	CapVectorSearcher   Capability = "vector_searcher"
	CapTimeSeriesReader Capability = "timeseries_reader"
	CapGraphReader      Capability = "graph_reader"
	CapCompleter        Capability = "completer"
	CapExporter         Capability = "exporter"
	CapWatcher          Capability = "watcher"
)

// CapReadOnly is a declared guarantee rather than an interface: the driver
// enforces Query.ReadOnly (and BrowseOptions.ReadOnly) as a hard guard, so a
// write sent with ReadOnly fails with ErrReadOnly and changes nothing,
// whatever the query text says: compound forms included (several
// statements in one text, a writable CTE, a function with side effects
// where the database can tell). The guarantee extends to Explainer.Explain
// with Query.ReadOnly: explaining a write never runs it, and an ANALYZE of
// a write fails with ErrReadOnly (or ErrUnsupported when the driver has no
// ANALYZE). A driver claims it by listing it in
// Info.Capabilities, and callers check it with HasCapability; driver.As and
// CapabilitiesOf do not apply. The AI assistant runs queries only on
// connections whose driver declares it. Under drivertest's Strict mode a
// declared CapReadOnly is probed with the fixture's writes.
const CapReadOnly Capability = "readonly"

// HasCapability reports whether info declares c in Info.Capabilities. It is
// the check for declared capabilities such as CapReadOnly; for an interface
// capability on a live value, use As.
func HasCapability(info Info, c Capability) bool { return slices.Contains(info.Capabilities, c) }

// capabilityEntry pairs a capability with its interface type.
type capabilityEntry struct {
	cap Capability
	typ reflect.Type
	// has reports whether c implements the interface.
	has func(c any) bool
}

func entry[T any](c Capability) capabilityEntry {
	return capabilityEntry{cap: c, typ: reflect.TypeFor[T](), has: func(v any) bool { _, ok := v.(T); return ok }}
}

// capabilityList is the ordered capability table. It is built once and never mutated.
var capabilityList = []capabilityEntry{
	entry[Browser](CapBrowser),
	entry[Mutator](CapMutator),
	entry[Explainer](CapExplainer),
	entry[Transactor](CapTransactor),
	entry[VectorSearcher](CapVectorSearcher),
	entry[TimeSeriesReader](CapTimeSeriesReader),
	entry[GraphReader](CapGraphReader),
	entry[Completer](CapCompleter),
	entry[Exporter](CapExporter),
	entry[Watcher](CapWatcher),
}

// capabilityOf maps each capability interface type to its Capability. It is
// built in its initializer and never mutated.
var capabilityOf = func() map[reflect.Type]Capability {
	m := make(map[reflect.Type]Capability, len(capabilityList))
	for _, e := range capabilityList {
		m[e.typ] = e.cap
	}
	return m
}()

// Supporter is implemented by Conns (notably plugin proxies) that satisfy more
// interfaces statically than they support at runtime.
type Supporter interface{ Supports(Capability) bool }

func supports(c any, cap Capability) bool {
	s, ok := c.(Supporter)
	return !ok || s.Supports(cap)
}

// As returns c as capability interface T when c implements T and, if c is a
// Supporter, reports support for T's capability. As[Browser](conn),
// As[Mutator](tx)… When T is not a capability interface, As is a plain type
// assertion. A nil c is never ok.
func As[T any](c any) (T, bool) {
	t, ok := c.(T)
	if !ok {
		var zero T
		return zero, false
	}
	if cap, isCap := capabilityOf[reflect.TypeFor[T]()]; isCap && !supports(c, cap) {
		var zero T
		return zero, false
	}
	return t, true
}

// CapabilitiesOf lists the capabilities c actually offers (for the TUI and
// `driver info`), in the order of the Cap constants.
func CapabilitiesOf(c any) []Capability {
	if c == nil {
		return nil
	}
	var out []Capability
	for _, e := range capabilityList {
		if e.has(c) && supports(c, e.cap) {
			out = append(out, e.cap)
		}
	}
	return out
}

// Browser pages through a collection with optional filter and sort.
type Browser interface {
	Browse(ctx context.Context, ref CollectionRef, opts BrowseOptions) (ResultSet, error)
}

// BrowseOptions controls a Browse call.
type BrowseOptions struct {
	Filter   Filter
	Sort     []SortKey
	Limit    int    // page size; 0 = driver default (100)
	Cursor   string // opaque; "" = first page; next page from Stats.NextCursor
	ReadOnly bool   // like Query.ReadOnly: Filter.Raw is native text and could call side-effecting functions
}

// SortKey orders a Browse by one field.
type SortKey struct {
	Field string
	Desc  bool
}

// Filter is a structured filter the driver translates to its own language, or a
// raw native filter fragment (a SQL WHERE body, a Mongo filter document…). If both
// are set, they are ANDed. Unsupported ops → ErrUnsupported.
type Filter struct {
	Conditions []Condition // ANDed
	Raw        string
}

// Condition compares one field with a value.
type Condition struct {
	Field string
	Op    Op
	Value Value // ignored for OpIsNull/OpNotNull; a List for OpIn
}

// Op is a comparison operator in a Condition.
type Op string

// The filter operators.
const (
	OpEq       Op = "eq"
	OpNe       Op = "ne"
	OpLt       Op = "lt"
	OpLe       Op = "le"
	OpGt       Op = "gt"
	OpGe       Op = "ge"
	OpContains Op = "contains"
	OpPrefix   Op = "prefix"
	OpIn       Op = "in"
	OpIsNull   Op = "is_null"
	OpNotNull  Op = "not_null"
)

// Mutator edits single rows/documents identified by key (primary key fields, _id, Redis key…).
// Update and Delete must affect exactly one row: zero → ErrNotFound, more → ErrConflict.
type Mutator interface {
	Insert(ctx context.Context, ref CollectionRef, values Object) (key Object, err error)
	Update(ctx context.Context, ref CollectionRef, key Object, changes Object) error
	Delete(ctx context.Context, ref CollectionRef, key Object) error
}

// Transactor begins transactions. A Tx is a Querier and may also satisfy Mutator or Explainer (via As).
type Transactor interface {
	Begin(ctx context.Context, opts TxOptions) (Tx, error)
}

// TxOptions configures Begin.
type TxOptions struct {
	ReadOnly  bool
	Isolation string // native name; "" = default
}

// Tx is an open transaction.
type Tx interface {
	Querier
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error // no-op after Commit
}

// Explainer returns a query plan.
type Explainer interface {
	Explain(ctx context.Context, q Query, opts ExplainOptions) (*Plan, error)
}

// ExplainOptions configures Explain.
type ExplainOptions struct {
	Analyze bool // executes the query; refused when q.ReadOnly and q mutates
}

// Plan is a query plan.
type Plan struct {
	Format string    // "text" | "json"
	Text   string    // native plan output, always set
	Root   *PlanNode // optional parsed tree
}

// PlanNode is one step of a parsed Plan.
type PlanNode struct {
	Label    string
	Detail   string
	Cost     float64 // driver units; 0 if unknown
	Rows     int64   // estimated rows; -1 if unknown
	Children []*PlanNode
}

// Completer suggests completions for query text.
type Completer interface {
	Complete(ctx context.Context, req CompletionRequest) ([]Completion, error)
}

// CatalogInvalidator is implemented by a connection that caches catalog
// lookups for its Completer. It is optional and not a listed Capability:
// find it with driver.As[driver.CatalogInvalidator](conn), a plain type
// assertion. A host calls Invalidate after anything that may change the
// catalog (a DDL or DML run, a commit, an applied edit, a reload) so the
// next completion sees fresh names. It must be cheap and safe to call
// concurrently with Complete.
type CatalogInvalidator interface{ Invalidate() }

// CompletionRequest asks for completions at a cursor.
type CompletionRequest struct {
	Text      string
	Cursor    int // byte offset
	Namespace string
}

// Completion is one suggestion.
type Completion struct {
	Label  string
	Kind   string // keyword namespace collection field function
	Detail string
	Insert string // text to insert; "" = Label
}

// Exporter writes a query result in a native format.
type Exporter interface {
	ExportFormats() []string // native formats, e.g. {"csv", "binary"}
	Export(ctx context.Context, q Query, format string, w io.Writer) error
}

// Watcher streams change events.
type Watcher interface {
	Watch(ctx context.Context, req WatchRequest) (<-chan Event, error) // channel closes when ctx ends
}

// WatchRequest selects what to watch.
type WatchRequest struct {
	Collection *CollectionRef // change stream on a collection, or
	Channel    string         // LISTEN channel / key pattern / topic
}

// Event is one change notification.
type Event struct {
	Time    time.Time
	Op      string // insert update delete notify …
	Channel string
	Key     Object
	Payload Value
	Err     error // terminal error, last event before close
}

// VectorSearcher finds nearest neighbours.
type VectorSearcher interface {
	Similar(ctx context.Context, ref CollectionRef, q VectorQuery) (ResultSet, error) // includes a "_distance" column
}

// VectorQuery is a nearest-neighbour search. The result's "_distance"
// column ranks nearest first under Metric: the Euclidean distance for
// MetricL2, 1 - cosine similarity for MetricCosine and the negated inner
// product for MetricInnerProduct.
type VectorQuery struct {
	Vector []float32
	// Text is embedded with ConnConfig.Embedder when Vector is empty (or by
	// the database, when it embeds natively). Without either the driver
	// returns ErrUnsupported with NeedEmbedderHint.
	Text   string
	Field  string // vector field; "" = default
	K      int
	Filter Filter
	Metric VectorMetric // "" = the driver's or collection's default; an unsupported one is ErrUnsupported
}

// VectorMetric names a vector distance.
type VectorMetric string

// The vector metrics.
const (
	MetricDefault      VectorMetric = ""
	MetricL2           VectorMetric = "l2"
	MetricCosine       VectorMetric = "cosine"
	MetricInnerProduct VectorMetric = "ip"
)

// TimeSeriesReader reads time ranges.
type TimeSeriesReader interface {
	Range(ctx context.Context, ref CollectionRef, q RangeQuery) (ResultSet, error) // columns: _time, [group fields…], value
}

// RangeQuery selects and aggregates a time range: the points with
// Start <= time < End (End is exclusive; a zero bound is open). With a
// Window, points are grouped into windows aligned to the Unix epoch in UTC,
// and each result row's _time is its window's start (not its end); windows
// with no points are omitted, never returned as NULL or zero rows. Rows come
// in time order.
type RangeQuery struct {
	Field     string
	Start     time.Time
	End       time.Time     // exclusive
	Window    time.Duration // 0 = raw points
	Aggregate string        // mean sum min max count last
	GroupBy   []string
	Filter    Filter
}

// GraphReader explores a graph.
type GraphReader interface {
	Neighbors(ctx context.Context, q GraphQuery) (*Graph, error)
}

// GraphQuery selects a neighbourhood.
type GraphQuery struct {
	Start  Value
	Labels []string
	Depth  int
	Limit  int
}

// Graph is a set of nodes and edges.
type Graph struct {
	Nodes []Node
	Edges []Edge
}

// Node is a graph vertex.
type Node struct {
	ID     string
	Labels []string
	Props  Object
}

// Edge is a graph edge.
type Edge struct {
	ID, From, To, Type string
	Props              Object
}
