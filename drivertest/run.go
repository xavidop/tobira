package drivertest

import (
	"context"
	"slices"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
)

// Config describes the driver under test and the database it runs against.
type Config struct {
	// Driver is the driver under test (for Info checks).
	Driver driver.Driver
	// Open returns a fresh Conn to a database already seeded with People.
	// Every call must return a Conn to the SAME seeded database: the kit opens
	// several Conns and reads one Conn's writes through another, so a private
	// per-connection store (such as a separate sqlite ":memory:" database per
	// Open) does not work. The kit closes every Conn it opens.
	Open func(t testing.TB) driver.Conn
	// OpenReadOnly opens the same database with ConnConfig.ReadOnly = true.
	OpenReadOnly func(t testing.TB) driver.Conn
	// OpenBadAuth attempts an open with wrong credentials. nil means the
	// driver has no authentication and the BadAuth subtest is skipped.
	OpenBadAuth func(ctx context.Context) error
	Fixture     Fixture
	// Strict makes the capability subtests mandatory: every capability listed
	// in Driver.Info().Capabilities MUST be exercised. A claimed capability
	// whose driver.As[T] fails, or whose required fixture input is missing
	// (TxWrite, TxRestore, CompleteAt, Watch, Vector, TimeSeries, Graph),
	// fails instead of skipping, and so does a widecolumn or search driver
	// without its kind fixture. Drivers in this repository run Strict.
	Strict bool
}

// Fixture holds the native queries and names the kit needs. Queries are in the
// driver's own language.
type Fixture struct {
	Namespace  string       // where the People collection lives
	Collection string       // name of the People collection
	SelectAll  driver.Query // all People rows ordered by id
	Missing    driver.Query // references a collection that does not exist
	Write      driver.Query // a mutating statement against People
	Slow       driver.Query // runs for at least 5s; zero = skip cancellation mid-query
	ParamQuery driver.Query // "People where id = <param>" with Params [Int(7)]; zero = no params (skip)

	// UnorderedSelectAll is for a store that cannot return a whole
	// collection in key order (DynamoDB: a scan is in hash order, and
	// PartiQL orders only within one partition). SelectAll results are then
	// matched to People by id instead of by position: every id must appear
	// exactly once, and each row is still compared column by column, with
	// the same row count and columns. A wrong, missing, extra or duplicated
	// row still fails. Leave it false wherever SelectAll can ORDER BY id.
	UnorderedSelectAll bool

	// TypeOverrides records a driver's declared storage difference from the
	// People fixture, such as InfluxDB having no Decimal or JSON type. The
	// key is a People column name ("score") or "<collection>.<column>"
	// ("tobira_people.score"); the longer form wins. Describe then expects
	// the override Type instead of the default, still compared strictly, and
	// the data check expects values consistent with it: a Decimal stored as
	// Float must match numerically within 1e-9, JSON stored as String must
	// be a String holding equal JSON. A Time column may be overridden to Int
	// (Unix nanoseconds, exactly equal to the fixture time's UnixNano) or to
	// String (RFC 3339, RFC3339Nano accepted; parsed and compared to the
	// fixture time by instant, not by text). The Int key id may be
	// overridden to String for a store whose keys must be strings (Cosmos
	// DB): it must then hold the exact decimal text ("7"), and the Browser
	// and Mutator subtests read ids from that text and write their probe
	// ids as text. An override is not a skip.
	//
	// Declare only real storage limits; never use it to hide a bug. The
	// zero value changes nothing.
	TypeOverrides map[string]driver.Type

	// InsertDefaults are members the kit adds to the People-shaped probe
	// row the Mutator suite inserts (also on the read-only Insert check),
	// for a store that cannot hold a row without them: a vector store whose
	// records all need a vector (Pinecone). They are appended after the
	// People members, in key order. A key that names a People column fails
	// the suite: a default never replaces a People value, so every check
	// still sees the probe's own values. The zero value changes nothing.
	InsertDefaults map[string]driver.Value

	NoPrimaryKey bool // the driver has no key concept: skip the PrimaryKey assertion in Describe

	// AutoIncrementKey, when set, is what Describe must report as
	// Field.AutoIncrement for People's key field id (as the driver's seed
	// declares it). Nil skips the check.
	AutoIncrementKey *bool

	// Capability inputs, only needed when the capability is claimed.
	TxWrite    driver.Query             // native write run inside a Tx (Transactor suite)
	CompleteAt driver.CompletionRequest // cursor after "from "; expects the People name (Completer suite)

	// CompoundWrites are writes hidden in a compound form the dialect
	// allows: a multi-statement text ("select 1; delete ..."), a writable
	// CTE, and so on. When the driver declares driver.CapReadOnly, the
	// ReadOnlyGuard subtest sends each with Query.ReadOnly and expects
	// ErrReadOnly and unchanged data. Optional.
	CompoundWrites []driver.Query

	// TxRestore is a native statement that undoes TxWrite (run outside a Tx,
	// after the commit check). Without it the Transactor commit check is
	// skipped, because the kit could not put the data back.
	TxRestore driver.Query

	// Optional inputs for the experimental capabilities. A nil pointer skips
	// that capability's subtest.
	Watch      *driver.WatchRequest
	Vector     *VectorFixture
	TimeSeries *TimeSeriesFixture
	Graph      *GraphFixture

	// Kind fixtures, which need no capability: a nil pointer skips that
	// subtest, except that under Strict a driver whose Info().Kind is
	// KindWideColumn must set WideColumn and one whose Kind is KindSearch
	// must set Search.
	WideColumn *WideColumnFixture
	Search     *SearchFixture
}

// VectorFixture drives the VectorSearcher subtest. KindVectorFixture builds
// one for the vector dataset.
type VectorFixture struct {
	Collection driver.CollectionRef
	Query      driver.VectorQuery
	MinRows    int // rows Similar must return at least; 0 means 1

	// IDField names the result column holding the row id; "" means "id".
	IDField string
	// WantTopID, when not NULL, is the exact nearest neighbour of Query:
	// the first row's id must equal it, with the same Type (a driver whose
	// ids are text sets a String here), and Query with K = 1 must return
	// exactly that one row. A NULL (zero) value skips both checks.
	WantTopID driver.Value
	// WantTopByMetric maps a metric to the nearest neighbour of Query under
	// it, compared as WantTopID is; each entry runs Query with that Metric. Fixtures whose metrics
	// disagree on the winner prove the metric is respected. Empty skips.
	WantTopByMetric map[driver.VectorMetric]driver.Value
}

// TimeSeriesFixture drives the TimeSeriesReader subtest.
// KindTimeSeriesFixture builds one for the time-series dataset.
type TimeSeriesFixture struct {
	Collection driver.CollectionRef
	Query      driver.RangeQuery
	MinRows    int // rows Range must return at least; 0 means 1

	// WantPoints maps an aggregate (mean, min, max, count…) to the exact
	// windows Query must return with that Aggregate: the same number of
	// rows, each with a Time _time equal to this window start and (within
	// 1e-9 relative) this value.
	// Empty skips the check.
	WantPoints map[string][]TimePoint
}

// TimePoint is one expected window of a TimeSeriesFixture.
type TimePoint struct {
	Time  time.Time
	Value float64
}

// GraphFixture drives the GraphReader subtest.
type GraphFixture struct {
	Query    driver.GraphQuery
	MinNodes int // nodes Neighbors must return at least; 0 means 1
}

// Run executes the conformance suite against the driver described by c. Each
// subtest opens its own Conn, so subtests are independent, and they run in a
// fixed order.
func Run(t *testing.T, c Config) {
	t.Helper()
	// Snapshot before anything runs, so only goroutines started by the suite
	// (and left behind) count as leaks.
	before := goleak.IgnoreCurrent()
	validate(t, c)
	subtests := slices.Concat(coreSubtests, capSubtests, kindSubtests, []subtest{
		{"NoGoroutineLeak", func(t *testing.T, _ Config) { checkNoLeak(t, before) }},
	})
	for _, s := range subtests {
		t.Run(s.name, func(t *testing.T) { s.fn(t, c) })
	}
}

type subtest struct {
	name string
	fn   func(t *testing.T, c Config)
}

func validate(t *testing.T, c Config) {
	t.Helper()
	switch {
	case c.Driver == nil:
		t.Fatal("drivertest.Config.Driver is nil: set it to the driver under test")
	case c.Open == nil:
		t.Fatal("drivertest.Config.Open is nil: it must return a Conn seeded with the People fixture")
	case c.OpenReadOnly == nil:
		t.Fatal("drivertest.Config.OpenReadOnly is nil: it must open the same database with ConnConfig.ReadOnly = true")
	case c.Fixture.Namespace == "":
		t.Fatal("drivertest.Fixture.Namespace is empty: name the namespace that holds People")
	case c.Fixture.Collection == "":
		t.Fatal("drivertest.Fixture.Collection is empty: name the People collection")
	case c.Fixture.SelectAll.Text == "":
		t.Fatal("drivertest.Fixture.SelectAll.Text is empty: give a native query returning all People rows ordered by id")
	case c.Fixture.Missing.Text == "":
		t.Fatal("drivertest.Fixture.Missing.Text is empty: give a native query on a collection that does not exist")
	case c.Fixture.Write.Text == "":
		t.Fatal("drivertest.Fixture.Write.Text is empty: give a native mutating statement against People")
	}
	if err := c.Fixture.checkInsertDefaults(); err != nil {
		t.Fatal(err)
	}
}

// Bool returns a pointer to b, for the optional Fixture fields.
func Bool(b bool) *bool { return &b }
