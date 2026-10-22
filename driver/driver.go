package driver

import (
	"context"
	"log/slog"
)

// Kind is the broad family of a database. The TUI uses it only to choose default
// views and keybindings; behaviour is capability-driven.
type Kind string

// The known database families.
const (
	KindSQL      Kind = "sql"
	KindDocument Kind = "document"
	KindKeyValue Kind = "keyvalue"
	// KindWideColumn is a partitioned wide-column store (Cassandra,
	// ScyllaDB). Describe reports CollectionSchema.PartitionKey and
	// ClusteringKey; the TUI marks those columns in the grid header.
	KindWideColumn Kind = "widecolumn"
	KindVector     Kind = "vector"
	KindTimeSeries Kind = "timeseries"
	KindGraph      Kind = "graph"
	// KindSearch is a search engine (Elasticsearch, OpenSearch).
	// Collections are indexes (CollectionIndex) described from their
	// mappings; a query result may carry a numeric _score column, which the
	// TUI renders as a relevance score.
	KindSearch Kind = "search"
	KindOther  Kind = "other"
)

// Info describes a driver. It is static: it must not depend on a connection.
type Info struct {
	Name         string       // registry key, e.g. "postgres"
	Kind         Kind         // database family
	Schemes      []string     // DSN schemes, e.g. {"postgres", "postgresql"}
	Dialect      string       // native language name for highlighting/AI, e.g. "postgresql", "sqlite", "mongodb-shell", "redis"
	Version      string       // driver version
	Capabilities []Capability // what Conns from this driver may implement (see As), plus declared guarantees such as CapReadOnly
}

// Driver opens connections. Implementations must be safe for concurrent use.
type Driver interface {
	Info() Info
	Open(ctx context.Context, cfg ConnConfig) (Conn, error)
}

// ConnConfig is everything needed to open a connection. The host resolves
// secrets before calling Open; drivers never see secret refs.
type ConnConfig struct {
	Name     string            // connection name from config (logs, errors); may be empty
	Driver   string            // explicit driver name; empty means "pick by DSN scheme"
	DSN      string            // native DSN; may or may not contain a password
	Password Secret            // overrides any password in DSN when non-empty
	ReadOnly bool              // open read-only at the protocol level where possible
	Options  map[string]string // driver-specific knobs (e.g. "sslmode", "busy_timeout")
	// Embedder turns text into vectors for VectorSearcher drivers; nil when
	// the host has none (no ai.embedder). The host sets it; drivers only
	// call it. RedactMessage ignores it.
	Embedder Embedder
	// Logger receives the driver's diagnostics, such as a warning that a
	// credential is sent without TLS. The host sets it from its --debug
	// log; nil means discard. Drivers log only through it (see Log), never
	// to stderr or the default slog logger, which would draw over the TUI.
	// It must never be given a secret. RedactMessage ignores it.
	Logger *slog.Logger
}

// discardLogger is what Log returns when ConnConfig.Logger is nil.
var discardLogger = slog.New(slog.DiscardHandler)

// Log returns c.Logger, or a logger that discards everything when it is
// nil, so a driver can log without checking.
func (c ConnConfig) Log() *slog.Logger {
	if c.Logger == nil {
		return discardLogger
	}
	return c.Logger
}

// Embedder turns text into vectors for VectorSearcher drivers. The host
// supplies it (built from ai.embedder); drivers never construct one. It must
// be safe for concurrent use. Embed returns one vector per text, in order;
// its errors are already redacted.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dimensions() int // 0 = unknown
}

// NeedEmbedderHint is the message of the ErrUnsupported a VectorSearcher
// returns for a text query when ConnConfig.Embedder is nil.
const NeedEmbedderHint = "text similarity needs ai.embedder"

// Querier runs native queries. Conn and Tx both satisfy it.
type Querier interface {
	Query(ctx context.Context, q Query) (ResultSet, error)
}

// Conn is an open connection (or pool). Safe for concurrent use.
type Conn interface {
	Querier
	Ping(ctx context.Context) error
	Close() error
	Namespaces(ctx context.Context) ([]Namespace, error)
	Collections(ctx context.Context, ns NamespaceRef) ([]Collection, error)
	Describe(ctx context.Context, ref CollectionRef) (*CollectionSchema, error)
}
