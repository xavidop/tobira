package api

import (
	"context"
	"time"

	"github.com/xavidop/tobira/driver"
)

// Driver, connection and configuration.
type (
	Driver     = driver.Driver
	Info       = driver.Info
	Kind       = driver.Kind
	ConnConfig = driver.ConnConfig
	Conn       = driver.Conn
	Querier    = driver.Querier
	Embedder   = driver.Embedder
	Secret     = driver.Secret
	Registry   = driver.Registry
)

// The database families.
const (
	KindSQL        = driver.KindSQL
	KindDocument   = driver.KindDocument
	KindKeyValue   = driver.KindKeyValue
	KindWideColumn = driver.KindWideColumn
	KindVector     = driver.KindVector
	KindTimeSeries = driver.KindTimeSeries
	KindGraph      = driver.KindGraph
	KindSearch     = driver.KindSearch
	KindOther      = driver.KindOther
)

// NeedEmbedderHint is driver.NeedEmbedderHint.
const NeedEmbedderHint = driver.NeedEmbedderHint

// Values.
type (
	Value  = driver.Value
	Type   = driver.Type
	Member = driver.Member
	Object = driver.Object
)

// The Value types.
const (
	TypeNull    = driver.TypeNull
	TypeBool    = driver.TypeBool
	TypeInt     = driver.TypeInt
	TypeFloat   = driver.TypeFloat
	TypeDecimal = driver.TypeDecimal
	TypeString  = driver.TypeString
	TypeBytes   = driver.TypeBytes
	TypeTime    = driver.TypeTime
	TypeJSON    = driver.TypeJSON
	TypeVector  = driver.TypeVector
	TypeList    = driver.TypeList
	TypeObject  = driver.TypeObject
	TypeUnknown = driver.TypeUnknown
)

// Null is driver.Null.
func Null() Value { return driver.Null() }

// Bool is driver.Bool.
func Bool(b bool) Value { return driver.Bool(b) }

// Int is driver.Int.
func Int(i int64) Value { return driver.Int(i) }

// Float is driver.Float.
func Float(f float64) Value { return driver.Float(f) }

// Decimal is driver.Decimal.
func Decimal(s string) Value { return driver.Decimal(s) }

// String is driver.String.
func String(s string) Value { return driver.String(s) }

// Bytes is driver.Bytes.
func Bytes(b []byte) Value { return driver.Bytes(b) }

// Time is driver.Time.
func Time(t time.Time) Value { return driver.Time(t) }

// JSON is driver.JSON.
func JSON(raw []byte) Value { return driver.JSON(raw) }

// Vector is driver.Vector.
func Vector(v []float32) Value { return driver.Vector(v) }

// List is driver.List.
func List(vs ...Value) Value { return driver.List(vs...) }

// ObjectValue is driver.ObjectValue.
func ObjectValue(ms ...Member) Value { return driver.ObjectValue(ms...) }

// Unknown is driver.Unknown.
func Unknown(nativeType string, raw []byte, display string) Value {
	return driver.Unknown(nativeType, raw, display)
}

// FromAny is driver.FromAny.
func FromAny(x any) (Value, error) { return driver.FromAny(x) }

// Queries and results.
type (
	Query          = driver.Query
	ResultSet      = driver.ResultSet
	MultiResultSet = driver.MultiResultSet
	Row            = driver.Row
	Column         = driver.Column
	Stats          = driver.Stats
)

// NewSliceResultSet is driver.NewSliceResultSet.
func NewSliceResultSet(cols []Column, rows []Row, stats Stats) ResultSet {
	return driver.NewSliceResultSet(cols, rows, stats)
}

// Collect is driver.Collect.
func Collect(ctx context.Context, rs ResultSet, max int) ([]Row, Stats, error) {
	return driver.Collect(ctx, rs, max)
}

// Catalog.
type (
	NamespaceRef     = driver.NamespaceRef
	CollectionRef    = driver.CollectionRef
	Namespace        = driver.Namespace
	CollectionType   = driver.CollectionType
	Collection       = driver.Collection
	CollectionSchema = driver.CollectionSchema
	Field            = driver.Field
	Index            = driver.Index
	ForeignKey       = driver.ForeignKey
)

// The collection types.
const (
	CollectionTable            = driver.CollectionTable
	CollectionView             = driver.CollectionView
	CollectionMaterializedView = driver.CollectionMaterializedView
	CollectionCollection       = driver.CollectionCollection
	CollectionMeasurement      = driver.CollectionMeasurement
	CollectionIndex            = driver.CollectionIndex
	CollectionKeyPattern       = driver.CollectionKeyPattern
	CollectionOther            = driver.CollectionOther
)

// Capabilities.
type (
	Capability         = driver.Capability
	Supporter          = driver.Supporter
	Browser            = driver.Browser
	BrowseOptions      = driver.BrowseOptions
	SortKey            = driver.SortKey
	Filter             = driver.Filter
	Condition          = driver.Condition
	Op                 = driver.Op
	Mutator            = driver.Mutator
	Transactor         = driver.Transactor
	TxOptions          = driver.TxOptions
	Tx                 = driver.Tx
	Explainer          = driver.Explainer
	ExplainOptions     = driver.ExplainOptions
	Plan               = driver.Plan
	PlanNode           = driver.PlanNode
	Completer          = driver.Completer
	CatalogInvalidator = driver.CatalogInvalidator
	CompletionRequest  = driver.CompletionRequest
	Completion         = driver.Completion
	Exporter           = driver.Exporter
	Watcher            = driver.Watcher
	WatchRequest       = driver.WatchRequest
	Event              = driver.Event
	VectorSearcher     = driver.VectorSearcher
	VectorQuery        = driver.VectorQuery
	VectorMetric       = driver.VectorMetric
	TimeSeriesReader   = driver.TimeSeriesReader
	RangeQuery         = driver.RangeQuery
	GraphReader        = driver.GraphReader
	GraphQuery         = driver.GraphQuery
	Graph              = driver.Graph
	Node               = driver.Node
	Edge               = driver.Edge
)

// The capabilities.
const (
	CapBrowser          = driver.CapBrowser
	CapMutator          = driver.CapMutator
	CapExplainer        = driver.CapExplainer
	CapTransactor       = driver.CapTransactor
	CapVectorSearcher   = driver.CapVectorSearcher
	CapTimeSeriesReader = driver.CapTimeSeriesReader
	CapGraphReader      = driver.CapGraphReader
	CapCompleter        = driver.CapCompleter
	CapExporter         = driver.CapExporter
	CapWatcher          = driver.CapWatcher
	CapReadOnly         = driver.CapReadOnly
)

// The filter operators.
const (
	OpEq       = driver.OpEq
	OpNe       = driver.OpNe
	OpLt       = driver.OpLt
	OpLe       = driver.OpLe
	OpGt       = driver.OpGt
	OpGe       = driver.OpGe
	OpContains = driver.OpContains
	OpPrefix   = driver.OpPrefix
	OpIn       = driver.OpIn
	OpIsNull   = driver.OpIsNull
	OpNotNull  = driver.OpNotNull
)

// The vector metrics.
const (
	MetricDefault      = driver.MetricDefault
	MetricL2           = driver.MetricL2
	MetricCosine       = driver.MetricCosine
	MetricInnerProduct = driver.MetricInnerProduct
)

// As is driver.As.
func As[T any](c any) (T, bool) { return driver.As[T](c) }

// CapabilitiesOf is driver.CapabilitiesOf.
func CapabilitiesOf(c any) []Capability { return driver.CapabilitiesOf(c) }

// HasCapability is driver.HasCapability.
func HasCapability(info Info, c Capability) bool { return driver.HasCapability(info, c) }

// Errors.
type Error = driver.Error

// The sentinel errors. They are the driver sentinels themselves, so
// errors.Is matches either name.
var (
	ErrNotFound      = driver.ErrNotFound
	ErrAuth          = driver.ErrAuth
	ErrPermission    = driver.ErrPermission
	ErrReadOnly      = driver.ErrReadOnly
	ErrUnsupported   = driver.ErrUnsupported
	ErrConnection    = driver.ErrConnection
	ErrSyntax        = driver.ErrSyntax
	ErrConstraint    = driver.ErrConstraint
	ErrConflict      = driver.ErrConflict
	ErrLimit         = driver.ErrLimit
	ErrUnknownDriver = driver.ErrUnknownDriver
)

// Wrap is driver.Wrap.
func Wrap(kind error, driverName, op string, err error) error {
	return driver.Wrap(kind, driverName, op, err)
}

// KindOf is driver.KindOf.
func KindOf(err error) error { return driver.KindOf(err) }

// KindName is driver.KindName.
func KindName(err error) string { return driver.KindName(err) }

// KindByName is driver.KindByName.
func KindByName(name string) error { return driver.KindByName(name) }

// Secrets, DSNs and redaction.

// NewSecret is driver.NewSecret.
func NewSecret(s string) Secret { return driver.NewSecret(s) }

// Scheme is driver.Scheme.
func Scheme(dsn string) string { return driver.Scheme(dsn) }

// RedactDSN is driver.RedactDSN.
func RedactDSN(dsn string) string { return driver.RedactDSN(dsn) }

// RedactMessage is driver.RedactMessage.
func RedactMessage(msg string, cfg ConnConfig) string { return driver.RedactMessage(msg, cfg) }

// HasPassword is driver.HasPassword.
func HasPassword(dsn string) bool { return driver.HasPassword(dsn) }

// StripPassword is driver.StripPassword.
func StripPassword(dsn string) (string, bool) { return driver.StripPassword(dsn) }

// NewRegistry is driver.NewRegistry: a private registry for an embedder.
func NewRegistry() *Registry { return driver.NewRegistry() }
