package drivertest

import (
	"slices"
	"sort"
	"time"

	"github.com/xavidop/tobira/driver"
)

// Names of the wide-column and search fixture collections. The Fake seeds
// them in namespace "main" (WithWideColumns and WithSearch); a driver's
// integration test seeds the same data into its database and points the
// Strict fixtures at it.
const (
	WideColumnCollection = "events"
	SearchCollection     = "articles"
)

// WideColumnPartition is the partition (tenant) the wide-column fixture's
// partition query reads.
const WideColumnPartition = "acme"

// SearchTerm is the word the search fixture's query looks for in the
// title and body fields.
const SearchTerm = "database"

// WideColumnStart is the time of the earliest event of the wide-column
// fixture. Every ts is a whole minute after it, so stores with
// millisecond timestamps keep it exactly.
func WideColumnStart() time.Time { return time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC) }

// WideColumnPartitionKey is the wide-column fixture's partition key.
func WideColumnPartitionKey() []string { return []string{"tenant"} }

// WideColumnClusteringKey is the wide-column fixture's clustering key: ts
// descending, then seq ascending.
func WideColumnClusteringKey() []driver.SortKey {
	return []driver.SortKey{{Field: "ts", Desc: true}, {Field: "seq"}}
}

// WideColumnDataset returns the wide-column fixture, table "events": rows
// of tenant (the partition key), ts and seq (the clustering columns), kind,
// value (a nullable float), attrs (a map of text, an Object) and tags (a
// set of text, a sorted List). Key is the whole primary key. The rows are
// in insertion order, which is not the clustering order, so a driver that
// returns a partition as stored fails the partition check. One row has no
// value and another no attrs: an unset column reads as NULL, the only
// "missing" a wide-column store has (Cassandra makes no NULL/unset
// distinction, and an empty collection reads as NULL, so the fixture has
// none).
func WideColumnDataset() Dataset {
	m := func(k, v string) driver.Member { return driver.Member{Key: k, Value: driver.String(v)} }
	at := func(minute int) driver.Value {
		return driver.Time(WideColumnStart().Add(time.Duration(minute) * time.Minute))
	}
	str := driver.String
	tags := func(ts ...string) driver.Value {
		vals := make([]driver.Value, len(ts))
		for i, s := range ts {
			vals[i] = str(s)
		}
		return driver.List(vals...)
	}
	row := func(tenant string, minute int, seq int64, kind string, value, attrs, tg driver.Value) driver.Row {
		return driver.Row{str(tenant), at(minute), driver.Int(seq), str(kind), value, attrs, tg}
	}
	return Dataset{
		Columns: []driver.Column{
			{Name: "tenant", Type: driver.TypeString, Key: true},
			{Name: "ts", Type: driver.TypeTime, Key: true},
			{Name: "seq", Type: driver.TypeInt, Key: true},
			{Name: "kind", Type: driver.TypeString},
			{Name: "value", Type: driver.TypeFloat},
			{Name: "attrs", Type: driver.TypeObject, NativeType: "map<text, text>"},
			{Name: "tags", Type: driver.TypeList, NativeType: "set<text>"},
		},
		Rows: []driver.Row{
			row("acme", 0, 1, "login", driver.Float(1.5), driver.ObjectValue(m("ip", "10.0.0.1")), tags("auth")),
			row("globex", 1, 1, "login", driver.Float(2), driver.ObjectValue(m("ip", "10.0.1.1")), tags("auth")),
			row("acme", 2, 1, "logout", driver.Float(0.25), driver.ObjectValue(m("ip", "10.0.0.1"), m("ua", "cli")), tags("auth", "cli")),
			row("acme", 0, 2, "query", driver.Null(), driver.ObjectValue(m("table", "users")), tags("db")),
			row("initech", 5, 1, "deploy", driver.Float(42), driver.Null(), tags("ci", "prod")),
			row("acme", 1, 1, "query", driver.Float(3.75), driver.ObjectValue(m("table", "orders")), tags("db", "slow")),
			row("globex", 0, 1, "signup", driver.Float(1), driver.ObjectValue(m("plan", "pro")), tags("billing")),
		},
		Key: []string{"tenant", "ts", "seq"},
	}
}

// WideColumnFixture drives the WideColumn subtest. KindWideColumnFixture
// builds one for the wide-column dataset.
type WideColumnFixture struct {
	Collection driver.CollectionRef
	// PartitionKey and ClusteringKey are what Describe must report,
	// exactly; PrimaryKey must be the partition key then the clustering
	// fields. Every WantColumns column must be a field of that Type.
	PartitionKey  []string
	ClusteringKey []driver.SortKey
	// Partition is a native query reading one whole partition with no
	// ORDER BY: the store's clustering order must hold. Its result must
	// equal WantRows (by WantColumns names, in order, EqualValue). Strict
	// requires it; outside Strict an empty Text skips the read.
	Partition   driver.Query
	WantColumns []driver.Column
	WantRows    []driver.Row
}

// KindWideColumnFixture returns the Strict wide-column fixture for the
// dataset seeded at ref. partition is the driver's native query for every
// row of tenant WideColumnPartition, without an ORDER BY clause (for CQL,
// SELECT * FROM events WHERE tenant = 'acme').
func KindWideColumnFixture(ref driver.CollectionRef, partition driver.Query) *WideColumnFixture {
	d := WideColumnDataset()
	var rows []driver.Row
	for _, r := range d.Rows {
		if s, _ := r[0].AsText(); s == WideColumnPartition {
			rows = append(rows, r)
		}
	}
	sortByKeys(d.Columns, rows, WideColumnClusteringKey())
	return &WideColumnFixture{
		Collection:    ref,
		PartitionKey:  WideColumnPartitionKey(),
		ClusteringKey: WideColumnClusteringKey(),
		Partition:     partition,
		WantColumns:   d.Columns,
		WantRows:      rows,
	}
}

// sortByKeys sorts rows (with columns cols) stably by keys; NULL sorts as
// the greatest value. A key naming no column is ignored.
func sortByKeys(cols []driver.Column, rows []driver.Row, keys []driver.SortKey) {
	sort.SliceStable(rows, func(a, b int) bool {
		for _, k := range keys {
			i := slices.IndexFunc(cols, func(c driver.Column) bool { return c.Name == k.Field })
			if i < 0 {
				continue
			}
			c := compareNullsHigh(rows[a][i], rows[b][i])
			if k.Desc {
				c = -c
			}
			if c != 0 {
				return c < 0
			}
		}
		return false
	})
}

// sortFields returns the field names of keys.
func sortFields(keys []driver.SortKey) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k.Field
	}
	return out
}

// SearchDocuments returns the search fixture, index "articles": six
// documents keyed by a string _id with title and body (text), tags (a
// keyword field holding a list), views (an integer), rating (a float,
// missing from one document) and author (an object of name and country,
// on three documents). SearchTerm appears, as a whole word and never in a
// plural or stemmed form, in exactly a1, a2 and a3; a1 has it in its
// short title and three times in its body, so it ranks first under BM25,
// TF-IDF or a plain term count.
func SearchDocuments() []driver.Object {
	m := func(k string, v driver.Value) driver.Member { return driver.Member{Key: k, Value: v} }
	str := driver.String
	author := func(name, country string) driver.Member {
		return m("author", driver.ObjectValue(m("name", str(name)), m("country", str(country))))
	}
	return []driver.Object{
		{
			m("_id", str("a1")), m("title", str("Database internals")),
			m("body", str("How a database stores pages: the database engine and its database files.")),
			m("tags", driver.List(str("storage"), str("engines"))), m("views", driver.Int(1200)), m("rating", driver.Float(4.5)),
			author("Ada", "UK"),
		},
		{
			m("_id", str("a2")), m("title", str("Terminal user interfaces")),
			m("body", str("Building a TUI in Go: a terminal explorer can browse any database you point it at, page by page.")),
			m("tags", driver.List(str("tui"), str("go"))), m("views", driver.Int(300)), m("rating", driver.Float(3.5)),
			author("Linus", "FI"),
		},
		{
			m("_id", str("a3")), m("title", str("Vector search")),
			m("body", str("Nearest neighbours over embeddings: a vector database answers similarity queries over many points.")),
			m("tags", driver.List(str("vectors"))), m("views", driver.Int(800)), m("rating", driver.Float(4.25)),
			author("Grace", "US"),
		},
		{
			m("_id", str("a4")), m("title", str("Gardening")),
			m("body", str("Tomatoes need sun and water.")),
			m("tags", driver.List(str("garden"))), m("views", driver.Int(50)), m("rating", driver.Float(2.5)),
		},
		{
			m("_id", str("a5")), m("title", str("Cooking pasta")),
			m("body", str("Salt the water generously and taste it.")),
			m("tags", driver.List(str("food"))), m("views", driver.Int(75)),
		},
		{
			m("_id", str("a6")), m("title", str("Indexes and mappings")),
			m("body", str("An inverted index maps terms to the documents that hold them.")),
			m("tags", driver.List(str("search"), str("indexing"))), m("views", driver.Int(410)), m("rating", driver.Float(4)),
		},
	}
}

// SearchFixture drives the Search subtest. KindSearchFixture builds one
// for the search documents.
type SearchFixture struct {
	Collection driver.CollectionRef
	// Type is the collection type Describe must report; "" skips the check.
	Type driver.CollectionType
	// Fields is the mapping Describe must report: each named field must be
	// present, with this Type unless it is TypeUnknown, and its nested
	// Fields are checked the same way. Where Fields (or a field's nested
	// Fields) are given, the mapping may hold no other field at that level,
	// except IDField at the top level; a field given without nested Fields
	// leaves its sub-fields (multi-fields, for one) unchecked.
	Fields []driver.Field
	// Query is the driver's native full-text query. Its result must carry
	// a numeric _score column, positive and best first; the IDField values
	// must be exactly WantIDs (in any order, with the same Type) and the
	// first must equal WantTopID.
	Query     driver.Query
	IDField   string // "" means "_id"
	WantIDs   []driver.Value
	WantTopID driver.Value
}

// KindSearchFixture returns the Strict search fixture for the documents
// seeded at ref. query is the driver's native query for SearchTerm over
// the title and body fields (for Elasticsearch, a multi_match on title
// and body). The index must map title and body as text, views as an
// integer, rating as a float and author as an object of name and country;
// tags may be mapped as any type.
func KindSearchFixture(ref driver.CollectionRef, query driver.Query) *SearchFixture {
	f := func(name string, t driver.Type, nested ...driver.Field) driver.Field {
		return driver.Field{Name: name, Type: t, Fields: nested}
	}
	return &SearchFixture{
		Collection: ref,
		Type:       driver.CollectionIndex,
		Fields: []driver.Field{
			f("title", driver.TypeString), f("body", driver.TypeString), f("tags", driver.TypeUnknown),
			f("views", driver.TypeInt), f("rating", driver.TypeFloat),
			f("author", driver.TypeObject, f("name", driver.TypeString), f("country", driver.TypeString)),
		},
		Query:     query,
		IDField:   "_id",
		WantIDs:   []driver.Value{driver.String("a1"), driver.String("a2"), driver.String("a3")},
		WantTopID: driver.String("a1"),
	}
}
