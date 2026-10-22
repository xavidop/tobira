package drivertest

import (
	"context"
	"hash/fnv"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/xavidop/tobira/driver"
)

// Names of the per-kind fixture collections. The Fake seeds each one in
// namespace "main" (WithDocuments, WithKeyValues, WithVectors and
// WithTimeSeries); a driver's integration test seeds the same data into its
// database and points the Strict fixtures at it.
const (
	DocumentsCollection  = "docs"
	KeyValueCollection   = "kv"
	VectorCollection     = "vecs"
	TimeSeriesCollection = "cpu"
)

// Documents returns the document fixture, a fresh copy on every call: five
// schemaless documents keyed by a string _id, with nested Objects, Lists,
// mixed types (age is an int in some documents and a string in another),
// an explicit null and missing fields.
func Documents() []driver.Object {
	m := func(k string, v driver.Value) driver.Member { return driver.Member{Key: k, Value: v} }
	obj := driver.ObjectValue
	str := driver.String
	return []driver.Object{
		{
			m("_id", str("d1")), m("name", str("Ada")), m("age", driver.Int(36)),
			m("address", obj(m("city", str("London")), m("zip", str("N1")))),
			m("tags", driver.List(str("math"), str("code"))),
		},
		{
			m("_id", str("d2")), m("name", str("Grace")), m("age", str("unknown")),
			m("tags", driver.List()), m("active", driver.Bool(true)),
		},
		{
			m("_id", str("d3")), m("name", str("Linus")),
			m("address", obj(m("city", str("Helsinki")), m("geo", obj(m("lat", driver.Float(60.17)), m("lon", driver.Float(24.94)))))),
			m("scores", driver.List(driver.Int(1), driver.Float(2.5), str("n/a"))),
		},
		{
			m("_id", str("d4")), m("name", driver.Null()), m("age", driver.Int(41)),
			m("nested", obj(m("deep", obj(m("deeper", obj(m("value", driver.Int(1)))))))),
		},
		{
			m("_id", str("d5")), m("title", str("only a title")),
		},
	}
}

// KeyValueDataset returns the key-value fixture: one row per key with
// columns _key, type, ttl (seconds, -1 = no expiry) and value, for keys of
// type string, hash (an Object), list, set (a sorted List) and zset (a List
// of {member, score} Objects in score order).
func KeyValueDataset() Dataset {
	m := func(k string, v driver.Value) driver.Member { return driver.Member{Key: k, Value: v} }
	str := driver.String
	row := func(key, typ string, ttl int64, v driver.Value) driver.Row {
		return driver.Row{str(key), str(typ), driver.Int(ttl), v}
	}
	return Dataset{
		Columns: []driver.Column{
			{Name: "_key", Type: driver.TypeString, Key: true},
			{Name: "type", Type: driver.TypeString},
			{Name: "ttl", Type: driver.TypeInt},
			{Name: "value", Type: driver.TypeUnknown, NativeType: "any"},
		},
		Rows: []driver.Row{
			row("user:1:name", "string", -1, str("Ada")),
			row("user:1", "hash", -1, driver.ObjectValue(m("name", str("Ada")), m("lang", str("en")))),
			row("queue:jobs", "list", 3600, driver.List(str("j1"), str("j2"), str("j3"))),
			row("tags:go", "set", -1, driver.List(str("db"), str("tui"))),
			row("leaderboard", "zset", -1, driver.List(
				driver.ObjectValue(m("member", str("bob")), m("score", driver.Float(1))),
				driver.ObjectValue(m("member", str("ada")), m("score", driver.Float(3.5))),
			)),
			row("session:abc", "string", 60, str("token-value")),
		},
		Key: []string{"_key"},
	}
}

// vectorQuery is the query vector of the vector fixture.
var vectorQuery = []float32{1, 0, 0, 0}

// VectorDataset returns the vector fixture: rows of id (int key), vec (a
// 4-dim Vector) and payload (an Object). Against the query vector
// (1, 0, 0, 0) of KindVectorFixture, the nearest row is id 1 under L2, id 2
// under cosine and id 3 under inner product, with no ties at the top.
func VectorDataset() Dataset {
	m := func(k string, v driver.Value) driver.Member { return driver.Member{Key: k, Value: v} }
	row := func(id int64, label string, v ...float32) driver.Row {
		return driver.Row{driver.Int(id), driver.Vector(v), driver.ObjectValue(m("label", driver.String(label)), m("n", driver.Int(id*10)))}
	}
	return Dataset{
		Columns: []driver.Column{
			{Name: "id", Type: driver.TypeInt, Key: true},
			{Name: "vec", Type: driver.TypeVector, NativeType: "vector(4)"},
			{Name: "payload", Type: driver.TypeObject},
		},
		Rows: []driver.Row{
			row(1, "near", 0.9, 0.3, 0, 0),
			row(2, "aligned", 5, 0.1, 0, 0),
			row(3, "large", 6, 6, 0, 0),
			row(4, "y-axis", 0, 1, 0, 0),
			row(5, "z-ish", 0.1, 0, 2, 0),
			row(6, "opposite", -1, 0, 0, 0.5),
		},
		Key: []string{"id"},
	}
}

// TimeSeriesStart is the time of the first point of the time-series
// fixture.
func TimeSeriesStart() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

const timeSeriesPoints = 120

// TimeSeriesDataset returns the time-series fixture, measurement "cpu": 120
// points one minute apart from TimeSeriesStart (two hours), with columns
// time, host (a tag alternating web-1 and web-2), usage_user and
// usage_system (floats).
func TimeSeriesDataset() Dataset {
	base := TimeSeriesStart()
	rows := make([]driver.Row, timeSeriesPoints)
	for i := range rows {
		host := "web-1"
		if i%2 == 1 {
			host = "web-2"
		}
		rows[i] = driver.Row{
			driver.Time(base.Add(time.Duration(i) * time.Minute)),
			driver.String(host),
			driver.Float(usageUser(i)),
			driver.Float(float64(i%10) / 4),
		}
	}
	return Dataset{
		Columns: []driver.Column{
			{Name: "time", Type: driver.TypeTime},
			{Name: "host", Type: driver.TypeString},
			{Name: "usage_user", Type: driver.TypeFloat},
			{Name: "usage_system", Type: driver.TypeFloat},
		},
		Rows: rows,
	}
}

func usageUser(i int) float64 { return float64((i*7)%23) + 0.5 }

// KindVectorFixture returns the Strict VectorSearcher fixture for the
// vector dataset seeded at ref: the L2 query with k = 3 and its exact
// nearest neighbours under each metric. A driver that lacks a metric
// deletes it from WantTopByMetric.
func KindVectorFixture(ref driver.CollectionRef) *VectorFixture {
	return &VectorFixture{
		Collection: ref,
		Query:      driver.VectorQuery{Vector: slices.Clone(vectorQuery), K: 3, Metric: driver.MetricL2},
		MinRows:    3,
		IDField:    "id",
		WantTopID:  driver.Int(1),
		WantTopByMetric: map[driver.VectorMetric]driver.Value{
			driver.MetricL2:           driver.Int(1),
			driver.MetricCosine:       driver.Int(2),
			driver.MetricInnerProduct: driver.Int(3),
		},
	}
}

// KindTimeSeriesFixture returns the Strict TimeSeriesReader fixture for the
// time-series dataset seeded at ref: usage_user over the two hours in
// 30-minute windows, with the expected mean, min, max and count of each
// window.
func KindTimeSeriesFixture(ref driver.CollectionRef) *TimeSeriesFixture {
	const window = 30 * time.Minute
	base := TimeSeriesStart()
	n := int(timeSeriesPoints * time.Minute / window)
	want := map[string][]TimePoint{}
	for w := range n {
		at := base.Add(time.Duration(w) * window)
		lo, hi, sum, count := math.Inf(1), math.Inf(-1), 0.0, 0
		for i := range timeSeriesPoints {
			if t := base.Add(time.Duration(i) * time.Minute); t.Before(at) || !t.Before(at.Add(window)) {
				continue
			}
			v := usageUser(i)
			lo, hi, sum, count = min(lo, v), max(hi, v), sum+v, count+1
		}
		want["mean"] = append(want["mean"], TimePoint{at, sum / float64(count)})
		want["min"] = append(want["min"], TimePoint{at, lo})
		want["max"] = append(want["max"], TimePoint{at, hi})
		want["count"] = append(want["count"], TimePoint{at, float64(count)})
	}
	return &TimeSeriesFixture{
		Collection: ref,
		Query:      driver.RangeQuery{Field: "usage_user", Start: base, End: base.Add(timeSeriesPoints * time.Minute), Window: window},
		MinRows:    n,
		WantPoints: want,
	}
}

// HashEmbedder is a deterministic test Embedder: each text maps to a vector
// derived from FNV hashes of the text, so equal texts embed equally and no
// network is involved. Dims 0 means 8.
type HashEmbedder struct{ Dims int }

// Dimensions returns the vector size.
func (h HashEmbedder) Dimensions() int {
	if h.Dims <= 0 {
		return 8
	}
	return h.Dims
}

// Embed returns one vector per text, each component in [-1, 1].
func (h HashEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([][]float32, len(texts))
	for i, s := range texts {
		v := make([]float32, h.Dimensions())
		for d := range v {
			f := fnv.New64a()
			_, _ = f.Write([]byte(strconv.Itoa(d) + "\x00" + s))
			v[d] = float32(f.Sum64()%2001)/1000 - 1
		}
		out[i] = v
	}
	return out, nil
}

var _ driver.Embedder = HashEmbedder{}
