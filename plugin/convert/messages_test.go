package convert_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/plugin/convert"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
)

func TestInfoRoundTrip(t *testing.T) {
	in := driver.Info{
		Name: "x", Kind: driver.KindSQL, Schemes: []string{"x", "xs"}, Dialect: "xql", Version: "1.2.3",
		Capabilities: []driver.Capability{driver.CapBrowser, driver.CapReadOnly},
	}
	p, err := convert.InfoToProto(in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := convert.InfoFromProto(wire(t, p, &pb.DriverInfo{}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("got %+v, want %+v", got, in)
	}
	if _, err := convert.InfoFromProto(nil); !errors.Is(err, convert.ErrInvalid) {
		t.Fatalf("nil info: %v, want ErrInvalid", err)
	}
	long := strings.Repeat("n", convert.MaxNameBytes+1)
	if _, err := convert.InfoFromProto(&pb.DriverInfo{Name: long}); !errors.Is(err, convert.ErrLimit) {
		t.Fatalf("long name: %v, want ErrLimit", err)
	}
	if _, err := convert.InfoFromProto(&pb.DriverInfo{Name: "x", Capabilities: []string{long}}); !errors.Is(err, convert.ErrLimit) {
		t.Fatalf("long capability: %v, want ErrLimit", err)
	}
}

func TestConnConfigRoundTrip(t *testing.T) {
	in := driver.ConnConfig{
		Name: "prod", Driver: "x", DSN: "x://u@h/db", Password: driver.NewSecret("s3cret"),
		ReadOnly: true, Options: map[string]string{"sslmode": "require"},
	}
	got := convert.ConnConfigFromProto(wire(t, convert.ConnConfigToProto(in), &pb.ConnConfig{}))
	if got.Name != in.Name || got.Driver != in.Driver || got.DSN != in.DSN || !got.ReadOnly ||
		got.Password.Reveal() != "s3cret" || !reflect.DeepEqual(got.Options, in.Options) {
		t.Fatalf("got %+v", got)
	}
	if empty := convert.ConnConfigFromProto(nil); !empty.Password.IsZero() || empty.DSN != "" {
		t.Fatalf("nil config: %+v", empty)
	}
}

func TestCatalogRoundTrip(t *testing.T) {
	nss := []driver.Namespace{{Name: "main", Label: "schema", Default: true, Comment: "c"}, {Name: "other"}}
	pns, err := convert.NamespacesToProto(nss)
	if err != nil {
		t.Fatal(err)
	}
	gotNS, err := convert.NamespacesFromProto(wire(t, &pb.NamespacesResponse{Namespaces: pns}, &pb.NamespacesResponse{}).GetNamespaces())
	if err != nil || !reflect.DeepEqual(gotNS, nss) {
		t.Fatalf("namespaces: %+v, %v", gotNS, err)
	}

	cols := []driver.Collection{
		{Ref: driver.CollectionRef{Namespace: "main", Name: "t"}, Type: driver.CollectionTable, RowEstimate: -1, Comment: "x"},
		{Ref: driver.CollectionRef{Namespace: "main", Name: "v"}, Type: driver.CollectionView, RowEstimate: 7},
	}
	pcs, err := convert.CollectionsToProto(cols)
	if err != nil {
		t.Fatal(err)
	}
	gotCols, err := convert.CollectionsFromProto(wire(t, &pb.CollectionsResponse{Collections: pcs}, &pb.CollectionsResponse{}).GetCollections())
	if err != nil || !reflect.DeepEqual(gotCols, cols) {
		t.Fatalf("collections: %+v, %v", gotCols, err)
	}

	schema := &driver.CollectionSchema{
		Ref:  driver.CollectionRef{Namespace: "main", Name: "t"},
		Type: driver.CollectionTable,
		Fields: []driver.Field{
			{Name: "id", NativeType: "integer", Type: driver.TypeInt, AutoIncrement: true},
			{Name: "doc", NativeType: "object", Type: driver.TypeObject, Nullable: true, Default: "'{}'", Generated: true, Comment: "d",
				Fields: []driver.Field{{Name: "inner", Type: driver.TypeString}}},
		},
		PrimaryKey:  []string{"id"},
		Indexes:     []driver.Index{{Name: "pk", Fields: []string{"id"}, Unique: true, Primary: true, Method: "btree"}},
		ForeignKeys: []driver.ForeignKey{{Name: "fk", Fields: []string{"doc"}, References: driver.CollectionRef{Namespace: "main", Name: "u"}, RefFields: []string{"id"}}},
		RowEstimate: 3, SizeBytes: -1, Schemaless: true,
		Sample:  []driver.Row{{driver.Int(1), driver.ObjectValue(driver.Member{Key: "inner", Value: driver.String("a")})}},
		Comment: "table", DDL: "CREATE TABLE t (...)",
	}
	ps, err := convert.SchemaToProto(schema)
	if err != nil {
		t.Fatal(err)
	}
	gotSchema, err := convert.SchemaFromProto(wire(t, ps, &pb.CollectionSchema{}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotSchema, schema) {
		t.Fatalf("schema: got %+v\nwant %+v", gotSchema, schema)
	}
	// A wide-column table: its partition and clustering keys cross the wire.
	wide := &driver.CollectionSchema{
		Ref:  driver.CollectionRef{Namespace: "ks", Name: "events"},
		Type: driver.CollectionTable,
		Fields: []driver.Field{
			{Name: "tenant", Type: driver.TypeString}, {Name: "ts", Type: driver.TypeTime}, {Name: "seq", Type: driver.TypeInt},
		},
		PrimaryKey:    []string{"tenant", "ts", "seq"},
		PartitionKey:  []string{"tenant"},
		ClusteringKey: []driver.SortKey{{Field: "ts", Desc: true}, {Field: "seq"}},
		RowEstimate:   -1, SizeBytes: -1,
	}
	pw, err := convert.SchemaToProto(wide)
	if err != nil {
		t.Fatal(err)
	}
	gotWide, err := convert.SchemaFromProto(wire(t, pw, &pb.CollectionSchema{}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotWide, wide) {
		t.Fatalf("wide-column schema: got %+v\nwant %+v", gotWide, wide)
	}
	if _, err := convert.SchemaFromProto(nil); !errors.Is(err, convert.ErrInvalid) {
		t.Fatalf("nil schema: %v", err)
	}
	if _, err := convert.SchemaToProto(nil); !errors.Is(err, convert.ErrInvalid) {
		t.Fatalf("nil schema encode: %v", err)
	}
}

func TestCatalogLimits(t *testing.T) {
	long := strings.Repeat("n", convert.MaxNameBytes+1)
	if _, err := convert.NamespacesFromProto([]*pb.Namespace{{Name: long}}); !errors.Is(err, convert.ErrLimit) {
		t.Fatalf("namespace: %v", err)
	}
	if _, err := convert.NamespacesFromProto([]*pb.Namespace{nil}); !errors.Is(err, convert.ErrInvalid) {
		t.Fatalf("nil namespace: %v", err)
	}
	if _, err := convert.CollectionsFromProto([]*pb.Collection{{Ref: &pb.CollectionRef{Name: long}}}); !errors.Is(err, convert.ErrLimit) {
		t.Fatalf("collection: %v", err)
	}
	if _, err := convert.CollectionsFromProto([]*pb.Collection{nil}); !errors.Is(err, convert.ErrInvalid) {
		t.Fatalf("nil collection: %v", err)
	}
	deep := &pb.Field{Name: "f"}
	for range convert.MaxDepth + 1 {
		deep = &pb.Field{Name: "f", Fields: []*pb.Field{deep}}
	}
	if _, err := convert.SchemaFromProto(&pb.CollectionSchema{Fields: []*pb.Field{deep}}); !errors.Is(err, convert.ErrLimit) {
		t.Fatalf("deep fields: %v", err)
	}
	if _, err := convert.SchemaFromProto(&pb.CollectionSchema{Fields: []*pb.Field{nil}}); !errors.Is(err, convert.ErrInvalid) {
		t.Fatalf("nil field: %v", err)
	}
	if _, err := convert.SchemaFromProto(&pb.CollectionSchema{Fields: []*pb.Field{{Name: "x", Type: 99}}}); !errors.Is(err, convert.ErrInvalid) {
		t.Fatalf("bad field type: %v", err)
	}
	if _, err := convert.SchemaFromProto(&pb.CollectionSchema{Sample: []*pb.Row{nil}}); !errors.Is(err, convert.ErrInvalid) {
		t.Fatalf("nil sample row: %v", err)
	}
	if _, err := convert.SchemaFromProto(&pb.CollectionSchema{PartitionKey: []string{long}}); !errors.Is(err, convert.ErrLimit) {
		t.Fatalf("long partition key field: %v", err)
	}
	if _, err := convert.SchemaFromProto(&pb.CollectionSchema{ClusteringKey: []*pb.SortKey{{Field: long}}}); !errors.Is(err, convert.ErrLimit) {
		t.Fatalf("long clustering key field: %v", err)
	}
	if _, err := convert.SchemaFromProto(&pb.CollectionSchema{ClusteringKey: []*pb.SortKey{nil}}); !errors.Is(err, convert.ErrInvalid) {
		t.Fatalf("nil clustering key: %v", err)
	}
}

func TestQueryMessagesRoundTrip(t *testing.T) {
	q := driver.Query{Text: "select ?", Params: []driver.Value{driver.Int(1), driver.String("x")}, Namespace: "main", Limit: 10, ReadOnly: true}
	pq, err := convert.QueryToProto(q)
	if err != nil {
		t.Fatal(err)
	}
	gotQ, err := convert.QueryFromProto(wire(t, pq, &pb.Query{}))
	if err != nil || !reflect.DeepEqual(gotQ, q) {
		t.Fatalf("query: %+v, %v", gotQ, err)
	}

	bo := driver.BrowseOptions{
		Filter: driver.Filter{Conditions: []driver.Condition{
			{Field: "a", Op: driver.OpEq, Value: driver.Int(1)},
			{Field: "b", Op: driver.OpIn, Value: driver.List(driver.String("x"), driver.String("y"))},
			{Field: "c", Op: driver.OpIsNull},
		}, Raw: "x > 1"},
		Sort:  []driver.SortKey{{Field: "a", Desc: true}, {Field: "b"}},
		Limit: 50, Cursor: "abc", ReadOnly: true,
	}
	pbo, err := convert.BrowseOptionsToProto(bo)
	if err != nil {
		t.Fatal(err)
	}
	gotBO, err := convert.BrowseOptionsFromProto(wire(t, pbo, &pb.BrowseOptions{}))
	if err != nil || !reflect.DeepEqual(gotBO, bo) {
		t.Fatalf("browse options: %+v, %v", gotBO, err)
	}

	plan := &driver.Plan{Format: "text", Text: "SCAN t", Root: &driver.PlanNode{Label: "scan", Detail: "t", Cost: 1.5, Rows: -1,
		Children: []*driver.PlanNode{{Label: "filter", Rows: 3}}}}
	pp, err := convert.PlanToProto(plan)
	if err != nil {
		t.Fatal(err)
	}
	gotPlan, err := convert.PlanFromProto(wire(t, pp, &pb.Plan{}))
	if err != nil || !reflect.DeepEqual(gotPlan, plan) {
		t.Fatalf("plan: %+v, %v", gotPlan, err)
	}
	flat := &driver.Plan{Format: "json", Text: "{}"}
	pf, _ := convert.PlanToProto(flat)
	if got, err := convert.PlanFromProto(wire(t, pf, &pb.Plan{})); err != nil || !reflect.DeepEqual(got, flat) {
		t.Fatalf("flat plan: %+v, %v", got, err)
	}

	if got := convert.ExplainOptionsFromProto(convert.ExplainOptionsToProto(driver.ExplainOptions{Analyze: true})); !got.Analyze {
		t.Fatal("explain options lost Analyze")
	}
	txo := driver.TxOptions{ReadOnly: true, Isolation: "serializable"}
	if got := convert.TxOptionsFromProto(convert.TxOptionsToProto(txo)); got != txo {
		t.Fatalf("tx options: %+v", got)
	}

	cr := driver.CompletionRequest{Text: "sel", Cursor: 3, Namespace: "main"}
	pcr := convert.CompleteRequestToProto(&pb.ConnHandle{Id: "c"}, cr)
	if got := convert.CompletionRequestFromProto(wire(t, pcr, &pb.CompleteRequest{})); got != cr {
		t.Fatalf("completion request: %+v", got)
	}
	comps := []driver.Completion{{Label: "select", Kind: "keyword", Detail: "d", Insert: "SELECT"}}
	pcomp, err := convert.CompletionsToProto(comps)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := convert.CompletionsFromProto(pcomp); err != nil || !reflect.DeepEqual(got, comps) {
		t.Fatalf("completions: %+v, %v", got, err)
	}
}

func TestAnalyticsMessagesRoundTrip(t *testing.T) {
	vq := driver.VectorQuery{Vector: []float32{1, 2}, Text: "hi", Field: "emb", K: 3, Metric: driver.MetricCosine,
		Filter: driver.Filter{Conditions: []driver.Condition{{Field: "a", Op: driver.OpGt, Value: driver.Float(1)}}}}
	pvq, err := convert.VectorQueryToProto(vq)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := convert.VectorQueryFromProto(wire(t, pvq, &pb.VectorQuery{})); err != nil || !reflect.DeepEqual(got, vq) {
		t.Fatalf("vector query: %+v, %v", got, err)
	}

	rq := driver.RangeQuery{Field: "v", Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		Window: time.Hour, Aggregate: "mean", GroupBy: []string{"host"}, Filter: driver.Filter{Raw: "x"}}
	prq, err := convert.RangeQueryToProto(rq)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := convert.RangeQueryFromProto(wire(t, prq, &pb.RangeQuery{})); err != nil || !reflect.DeepEqual(got, rq) {
		t.Fatalf("range query: %+v, %v", got, err)
	}
	open := driver.RangeQuery{Field: "v"}
	pOpen, _ := convert.RangeQueryToProto(open)
	if pOpen.GetStart() != nil || pOpen.GetEnd() != nil {
		t.Fatal("open bounds must be unset")
	}
	if got, err := convert.RangeQueryFromProto(pOpen); err != nil || !got.Start.IsZero() || !got.End.IsZero() {
		t.Fatalf("open range: %+v, %v", got, err)
	}

	gq := driver.GraphQuery{Start: driver.String("n1"), Labels: []string{"knows"}, Depth: 2, Limit: 10}
	pgq, err := convert.GraphQueryToProto(gq)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := convert.GraphQueryFromProto(wire(t, pgq, &pb.GraphQuery{})); err != nil || !reflect.DeepEqual(got, gq) {
		t.Fatalf("graph query: %+v, %v", got, err)
	}
	g := &driver.Graph{
		Nodes: []driver.Node{{ID: "n1", Labels: []string{"P"}, Props: driver.Object{{Key: "name", Value: driver.String("a")}}}},
		Edges: []driver.Edge{{ID: "e1", From: "n1", To: "n2", Type: "knows", Props: driver.Object{{Key: "w", Value: driver.Int(1)}}}},
	}
	pg, err := convert.GraphToProto(g)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := convert.GraphFromProto(wire(t, pg, &pb.Graph{})); err != nil || !reflect.DeepEqual(got, g) {
		t.Fatalf("graph: %+v, %v", got, err)
	}

	ref := driver.CollectionRef{Namespace: "main", Name: "t"}
	wr := driver.WatchRequest{Collection: &ref, Channel: "ch"}
	pwr := convert.WatchRequestToProto(&pb.ConnHandle{Id: "c"}, wr)
	if got, err := convert.WatchRequestFromProto(wire(t, pwr, &pb.WatchRequest{})); err != nil || !reflect.DeepEqual(got, wr) {
		t.Fatalf("watch request: %+v, %v", got, err)
	}
	if got, err := convert.WatchRequestFromProto(&pb.WatchRequest{Channel: "c"}); err != nil || got.Collection != nil {
		t.Fatalf("channel watch: %+v, %v", got, err)
	}
}

func TestEventRoundTrip(t *testing.T) {
	ev := driver.Event{Time: time.Date(2026, 1, 1, 0, 0, 0, 5, time.UTC), Op: "insert", Channel: "main.t",
		Key: driver.Object{{Key: "id", Value: driver.Int(1)}}, Payload: driver.String("p")}
	pe, err := convert.EventToProto(ev, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := convert.EventFromProto(wire(t, pe, &pb.Event{}))
	if err != nil || !reflect.DeepEqual(got, ev) {
		t.Fatalf("event: %+v, %v", got, err)
	}

	failed := driver.Event{Op: "error", Err: driver.Wrap(driver.ErrConnection, "x", "watch", errors.New("lost pw=hunter22"))}
	pf, err := convert.EventToProto(failed, func(s string) string { return strings.ReplaceAll(s, "hunter22", "xxxxx") })
	if err != nil {
		t.Fatal(err)
	}
	gotF, err := convert.EventFromProto(wire(t, pf, &pb.Event{}))
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(gotF.Err, driver.ErrConnection) || strings.Contains(gotF.Err.Error(), "hunter22") {
		t.Fatalf("event error: %v", gotF.Err)
	}
	if _, err := convert.EventFromProto(nil); !errors.Is(err, convert.ErrInvalid) {
		t.Fatalf("nil event: %v", err)
	}
}

func TestQueryMessageLimits(t *testing.T) {
	if _, err := convert.QueryFromProto(&pb.Query{Params: []*pb.Value{nil}}); !errors.Is(err, convert.ErrInvalid) {
		t.Fatalf("nil param: %v", err)
	}
	if _, err := convert.BrowseOptionsFromProto(&pb.BrowseOptions{Filter: &pb.Filter{Conditions: []*pb.Condition{nil}}}); !errors.Is(err, convert.ErrInvalid) {
		t.Fatalf("nil condition: %v", err)
	}
	if _, err := convert.BrowseOptionsFromProto(&pb.BrowseOptions{Sort: []*pb.SortKey{nil}}); !errors.Is(err, convert.ErrInvalid) {
		t.Fatalf("nil sort key: %v", err)
	}
	deep := &pb.PlanNode{}
	for range convert.MaxDepth + 1 {
		deep = &pb.PlanNode{Children: []*pb.PlanNode{deep}}
	}
	if _, err := convert.PlanFromProto(&pb.Plan{Root: deep}); !errors.Is(err, convert.ErrLimit) {
		t.Fatalf("deep plan: %v", err)
	}
	if _, err := convert.PlanFromProto(nil); !errors.Is(err, convert.ErrInvalid) {
		t.Fatalf("nil plan: %v", err)
	}
	if _, err := convert.PlanToProto(nil); !errors.Is(err, convert.ErrInvalid) {
		t.Fatalf("nil plan encode: %v", err)
	}
	if _, err := convert.GraphFromProto(&pb.Graph{Nodes: []*pb.Node{nil}}); !errors.Is(err, convert.ErrInvalid) {
		t.Fatalf("nil node: %v", err)
	}
	if _, err := convert.CompletionsFromProto([]*pb.Completion{nil}); !errors.Is(err, convert.ErrInvalid) {
		t.Fatalf("nil completion: %v", err)
	}
}
