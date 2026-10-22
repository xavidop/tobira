package driver

// NamespaceRef identifies a namespace (schema, database, keyspace, db number).
type NamespaceRef struct{ Name string }

// CollectionRef identifies a collection within a namespace.
type CollectionRef struct{ Namespace, Name string }

// Namespace is one level of grouping inside a connection.
type Namespace struct {
	Name    string
	Label   string // what this database calls it: "schema", "database", "keyspace", "db"
	Default bool   // the connection's current/default namespace
	Comment string
}

// CollectionType is the kind of a collection.
type CollectionType string

const (
	CollectionTable            CollectionType = "table"
	CollectionView             CollectionType = "view"
	CollectionMaterializedView CollectionType = "materialized_view"
	CollectionCollection       CollectionType = "collection"
	CollectionMeasurement      CollectionType = "measurement"
	CollectionIndex            CollectionType = "index"
	CollectionKeyPattern       CollectionType = "key_pattern"
	CollectionOther            CollectionType = "other"
)

// Collection is a listing entry for a table, view, collection and so on.
type Collection struct {
	Ref         CollectionRef
	Type        CollectionType
	RowEstimate int64 // -1 = unknown
	Comment     string
}

// CollectionSchema is the full description of one collection.
type CollectionSchema struct {
	Ref        CollectionRef
	Type       CollectionType
	Fields     []Field
	PrimaryKey []string
	// PartitionKey and ClusteringKey describe a wide-column table
	// (KindWideColumn, such as Cassandra): the columns that pick the
	// partition, then the columns that order rows inside it, each with its
	// clustering order. PrimaryKey still lists the whole key, partition
	// columns first, then the clustering columns in order. Both are nil for
	// other stores.
	PartitionKey  []string
	ClusteringKey []SortKey
	Indexes       []Index
	ForeignKeys   []ForeignKey // also feeds the AI schema-context "FK neighbours"
	RowEstimate   int64
	SizeBytes     int64 // -1 = unknown
	Schemaless    bool  // fields were inferred from Sample
	Sample        []Row // optional (schemaless stores); host strips it before sending to a model unless allowed
	Comment       string
	DDL           string // optional native definition
}

// Field describes one column or document field.
type Field struct {
	Name       string
	NativeType string // as the database spells it: "timestamp with time zone"
	Type       Type   // normalised value type
	Nullable   bool
	Default    string // native default expression; "" = none
	Generated  bool
	// AutoIncrement says the database assigns the value when an insert
	// leaves it out: a serial or identity column, an AUTO_INCREMENT
	// column, SQLite's INTEGER PRIMARY KEY (the rowid). Such a field is
	// writable (unlike Generated) but never required on insert.
	AutoIncrement bool
	Comment       string
	Fields        []Field // nested fields for documents
}

// Index describes a secondary or primary index.
type Index struct {
	Name    string
	Fields  []string
	Unique  bool
	Primary bool
	Method  string
}

// ForeignKey describes a reference to another collection.
type ForeignKey struct {
	Name       string
	Fields     []string
	References CollectionRef
	RefFields  []string
}
