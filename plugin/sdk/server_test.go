package sdk_test

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"errors"
	"log/slog"
	"sync/atomic"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/plugin/convert"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
	"github.com/xavidop/tobira/plugin/sdk"
	"google.golang.org/grpc/status"
)

func capNames(cs []driver.Capability) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = string(c)
	}
	return out
}

func TestInfo(t *testing.T) {
	f := newFake()
	c, _ := start(t, f.Driver())
	resp, err := c.Info(context.Background(), &pb.InfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := convert.InfoFromProto(resp.GetInfo())
	if err != nil {
		t.Fatal(err)
	}
	if want := f.Driver().Info(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Info = %+v, want %+v", got, want)
	}
}

func TestOpenAdvertisesWhatTheConnOffers(t *testing.T) {
	f := newFake()
	c, _ := start(t, f.Driver())
	resp := open(t, c, &pb.ConnConfig{})
	if want := capNames(driver.CapabilitiesOf(direct(t, f.Driver()))); !slices.Equal(resp.GetCapabilities(), want) {
		t.Fatalf("capabilities %v, want %v", resp.GetCapabilities(), want)
	}
	if !slices.Equal(resp.GetExportFormats(), []string{"csv", "ndjson"}) {
		t.Fatalf("export formats %v", resp.GetExportFormats())
	}
	if resp.GetCatalogInvalidator() {
		t.Fatal("the Fake is not a CatalogInvalidator")
	}

	// Two Opens give two distinct handles.
	if other := open(t, c, &pb.ConnConfig{}); other.GetConn().GetId() == resp.GetConn().GetId() {
		t.Fatal("handles must be unique")
	}
}

func TestOpenPassesTheConfig(t *testing.T) {
	var got driver.ConnConfig
	d := &recordingDriver{Driver: newFake().Driver(), onOpen: func(cfg driver.ConnConfig) { got = cfg }}
	c, _ := start(t, d)
	open(t, c, &pb.ConnConfig{Name: "prod", Driver: "fake", Dsn: "fake://h", Password: "pw", ReadOnly: true, Options: map[string]string{"k": "v"}})
	if got.Name != "prod" || got.Driver != "fake" || got.DSN != "fake://h" || got.Password.Reveal() != "pw" ||
		!got.ReadOnly || got.Options["k"] != "v" {
		t.Fatalf("driver saw %+v", got)
	}
	if got.Logger == nil {
		t.Fatal("the driver must get a Logger (stderr), never nil")
	}
	if got.Embedder != nil {
		t.Fatal("a plugin has no embedder")
	}
}

func TestCatalogRPCs(t *testing.T) {
	f := newFake()
	c, _ := start(t, f.Driver())
	ref0 := direct(t, f.Driver())
	ctx := context.Background()
	h := open(t, c, &pb.ConnConfig{}).GetConn()

	if _, err := c.Ping(ctx, &pb.PingRequest{Conn: h}); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	nsResp, err := c.Namespaces(ctx, &pb.NamespacesRequest{Conn: h})
	if err != nil {
		t.Fatal(err)
	}
	gotNS, _ := convert.NamespacesFromProto(nsResp.GetNamespaces())
	wantNS, _ := ref0.Namespaces(ctx)
	if !reflect.DeepEqual(gotNS, wantNS) {
		t.Fatalf("Namespaces = %+v, want %+v", gotNS, wantNS)
	}

	colResp, err := c.Collections(ctx, &pb.CollectionsRequest{Conn: h, Namespace: "main"})
	if err != nil {
		t.Fatal(err)
	}
	gotCols, _ := convert.CollectionsFromProto(colResp.GetCollections())
	wantCols, _ := ref0.Collections(ctx, driver.NamespaceRef{Name: "main"})
	if !reflect.DeepEqual(gotCols, wantCols) {
		t.Fatalf("Collections = %+v, want %+v", gotCols, wantCols)
	}

	dResp, err := c.Describe(ctx, &pb.DescribeRequest{Conn: h, Ref: ref(people)})
	if err != nil {
		t.Fatal(err)
	}
	gotSchema, err := convert.SchemaFromProto(dResp.GetSchema())
	if err != nil {
		t.Fatal(err)
	}
	wantSchema, _ := ref0.Describe(ctx, people)
	if !reflect.DeepEqual(gotSchema, wantSchema) {
		t.Fatalf("Describe = %+v, want %+v", gotSchema, wantSchema)
	}

	if _, err := c.Close(ctx, &pb.CloseRequest{Conn: h}); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestQueryStreamsColumnsBatchesStats(t *testing.T) {
	f := newFake()
	cols := []driver.Column{{Name: "n", Type: driver.TypeInt}}
	rows := make([]driver.Row, 600)
	for i := range rows {
		rows[i] = driver.Row{driver.Int(int64(i))}
	}
	f.OnQuery("six hundred", func(driver.Query) (driver.ResultSet, error) {
		return driver.NewSliceResultSet(cols, rows, driver.Stats{RowsAffected: -1}), nil
	})
	c, _ := start(t, f.Driver())
	h := open(t, c, &pb.ConnConfig{}).GetConn()
	stream, err := c.Query(context.Background(), &pb.QueryRequest{Conn: h, Query: &pb.Query{Text: "six hundred"}})
	if err != nil {
		t.Fatal(err)
	}
	var shape []string
	for {
		fr, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch x := fr.GetFrame().(type) {
		case *pb.ResultFrame_Columns:
			shape = append(shape, "columns")
		case *pb.ResultFrame_Rows:
			shape = append(shape, "rows:"+itoa(len(x.Rows.GetRows())))
		case *pb.ResultFrame_Stats:
			shape = append(shape, "stats")
		}
	}
	if want := []string{"columns", "rows:256", "rows:256", "rows:88", "stats"}; !slices.Equal(shape, want) {
		t.Fatalf("frames %v, want %v", shape, want)
	}
}

func itoa(n int) string {
	b := []byte{}
	for {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
		if n == 0 {
			return string(b)
		}
	}
}

func TestQueryMatchesTheDriver(t *testing.T) {
	f := newFake()
	c, _ := start(t, f.Driver())
	ref0 := direct(t, f.Driver())
	ctx := context.Background()
	h := open(t, c, &pb.ConnConfig{}).GetConn()

	for _, q := range []driver.Query{
		{Text: "select * from tobira_people order by id"},
		{Text: "select * from tobira_people where id = $1", Params: []driver.Value{driver.Int(7)}},
		{Text: "select * from tobira_people order by id", Limit: 5},
	} {
		stream, err := c.Query(ctx, &pb.QueryRequest{Conn: h, Query: query(t, q)})
		if err != nil {
			t.Fatal(err)
		}
		got := collect(t, stream.Recv)
		want := collectDirect(t, func() (driver.ResultSet, error) { return ref0.Query(ctx, q) })
		sameResult(t, q.Text, got, want)
	}
}

func TestQueryMultiResultSet(t *testing.T) {
	f := newFake()
	c, _ := start(t, f.Driver())
	h := open(t, c, &pb.ConnConfig{}).GetConn()
	stream, err := c.Query(context.Background(), &pb.QueryRequest{Conn: h, Query: &pb.Query{
		Text: "select * from tobira_people where id = 1; select * from tobira_people where id = 2",
	}})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := convert.NewFrameResultSet(context.Background(), stream.Recv, func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rs.Close() }()
	var ids []int64
	for {
		for {
			row, ok, err := rs.Next(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			id, _ := row[0].AsInt()
			ids = append(ids, id)
		}
		more, err := rs.NextResultSet(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
	}
	if !slices.Equal(ids, []int64{1, 2}) {
		t.Fatalf("ids %v, want [1 2]", ids)
	}
}

func TestBrowse(t *testing.T) {
	f := newFake()
	c, _ := start(t, f.Driver())
	ref0 := direct(t, f.Driver())
	ctx := context.Background()
	h := open(t, c, &pb.ConnConfig{}).GetConn()

	opts := driver.BrowseOptions{
		Filter: driver.Filter{Conditions: []driver.Condition{{Field: "active", Op: driver.OpEq, Value: driver.Bool(true)}}},
		Sort:   []driver.SortKey{{Field: "id", Desc: true}},
		Limit:  7,
	}
	popts, err := convert.BrowseOptionsToProto(opts)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := c.Browse(ctx, &pb.BrowseRequest{Conn: h, Ref: ref(people), Options: popts})
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, stream.Recv)
	want := collectDirect(t, func() (driver.ResultSet, error) { return ref0.(driver.Browser).Browse(ctx, people, opts) })
	sameResult(t, "browse", got, want)
	if got.stats.NextCursor == "" {
		t.Fatal("the first page must carry a next cursor")
	}
}

func TestMutator(t *testing.T) {
	f := newFake()
	c, _ := start(t, f.Driver())
	ctx := context.Background()
	h := open(t, c, &pb.ConnConfig{}).GetConn()

	ins, err := c.Insert(ctx, &pb.InsertRequest{Conn: h, Ref: ref(people), Values: obj(t, driver.Object{
		{Key: "id", Value: driver.Int(1000)}, {Key: "name", Value: driver.String("New")}, {Key: "email", Value: driver.String("new@example.com")},
	})})
	if err != nil {
		t.Fatal(err)
	}
	key, err := convert.ObjectFromProto(ins.GetKey())
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := key.Get("id"); !sameValue(id, driver.Int(1000)) {
		t.Fatalf("insert key %v", key)
	}
	if _, err := c.Update(ctx, &pb.UpdateRequest{Conn: h, Ref: ref(people), Key: ins.GetKey(),
		Changes: obj(t, driver.Object{{Key: "name", Value: driver.String("Renamed")}})}); err != nil {
		t.Fatal(err)
	}
	rows := queryRows(t, c, h, "select * from tobira_people where id = 1000")
	if len(rows) != 1 || !sameValue(rows[0][1], driver.String("Renamed")) {
		t.Fatalf("after update: %v", rows)
	}
	if _, err := c.Delete(ctx, &pb.DeleteRequest{Conn: h, Ref: ref(people), Key: ins.GetKey()}); err != nil {
		t.Fatal(err)
	}
	if rows := queryRows(t, c, h, "select * from tobira_people where id = 1000"); len(rows) != 0 {
		t.Fatalf("after delete: %v", rows)
	}
	_, err = c.Delete(ctx, &pb.DeleteRequest{Conn: h, Ref: ref(people), Key: ins.GetKey()})
	wantStatus(t, "delete missing row", err, codes.NotFound, "not_found")
}

func queryRows(t *testing.T, c pb.DriverClient, h *pb.ConnHandle, text string) []driver.Row {
	t.Helper()
	stream, err := c.Query(context.Background(), &pb.QueryRequest{Conn: h, Query: &pb.Query{Text: text}})
	if err != nil {
		t.Fatal(err)
	}
	return collect(t, stream.Recv).rows
}

func TestExplainAndComplete(t *testing.T) {
	f := newFake()
	c, _ := start(t, f.Driver())
	ref0 := direct(t, f.Driver())
	ctx := context.Background()
	h := open(t, c, &pb.ConnConfig{}).GetConn()

	q := driver.Query{Text: "select * from tobira_people"}
	resp, err := c.Explain(ctx, &pb.ExplainRequest{Conn: h, Query: query(t, q), Options: &pb.ExplainOptions{Analyze: true}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := convert.PlanFromProto(resp.GetPlan())
	want, _ := ref0.(driver.Explainer).Explain(ctx, q, driver.ExplainOptions{Analyze: true})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Explain = %+v, want %+v", got, want)
	}

	req := driver.CompletionRequest{Text: "select * from tobira_", Cursor: 21}
	cResp, err := c.Complete(ctx, convert.CompleteRequestToProto(h, req))
	if err != nil {
		t.Fatal(err)
	}
	gotC, _ := convert.CompletionsFromProto(cResp.GetCompletions())
	wantC, _ := ref0.(driver.Completer).Complete(ctx, req)
	if len(gotC) == 0 || !reflect.DeepEqual(gotC, wantC) {
		t.Fatalf("Complete = %+v, want %+v", gotC, wantC)
	}
}

func TestInvalidate(t *testing.T) {
	d := &invalidatingDriver{Driver: newFake().Driver()}
	c, _ := start(t, d)
	resp := open(t, c, &pb.ConnConfig{})
	if !resp.GetCatalogInvalidator() {
		t.Fatal("OpenResponse must report the CatalogInvalidator")
	}
	if _, err := c.Invalidate(context.Background(), &pb.InvalidateRequest{Conn: resp.GetConn()}); err != nil {
		t.Fatal(err)
	}
	if d.calls.Load() != 1 {
		t.Fatalf("Invalidate reached the driver %d times", d.calls.Load())
	}
}

func TestAnalyticsRPCs(t *testing.T) {
	f := newFake()
	c, _ := start(t, f.Driver())
	ref0 := direct(t, f.Driver())
	ctx := context.Background()
	h := open(t, c, &pb.ConnConfig{}).GetConn()

	vq := drivertest.KindVectorFixture(vectors).Query
	pvq, err := convert.VectorQueryToProto(vq)
	if err != nil {
		t.Fatal(err)
	}
	sim, err := c.Similar(ctx, &pb.SimilarRequest{Conn: h, Ref: ref(vectors), Query: pvq})
	if err != nil {
		t.Fatal(err)
	}
	sameResult(t, "similar", collect(t, sim.Recv), collectDirect(t, func() (driver.ResultSet, error) { return ref0.(driver.VectorSearcher).Similar(ctx, vectors, vq) }))

	rq := drivertest.KindTimeSeriesFixture(cpu).Query
	prq, err := convert.RangeQueryToProto(rq)
	if err != nil {
		t.Fatal(err)
	}
	rng, err := c.Range(ctx, &pb.RangeRequest{Conn: h, Ref: ref(cpu), Query: prq})
	if err != nil {
		t.Fatal(err)
	}
	sameResult(t, "range", collect(t, rng.Recv), collectDirect(t, func() (driver.ResultSet, error) { return ref0.(driver.TimeSeriesReader).Range(ctx, cpu, rq) }))

	gq := driver.GraphQuery{Start: driver.Int(1), Depth: 1}
	pgq, _ := convert.GraphQueryToProto(gq)
	nResp, err := c.Neighbors(ctx, &pb.NeighborsRequest{Conn: h, Query: pgq})
	if err != nil {
		t.Fatal(err)
	}
	gotG, err := convert.GraphFromProto(nResp.GetGraph())
	if err != nil {
		t.Fatal(err)
	}
	wantG, _ := ref0.(driver.GraphReader).Neighbors(ctx, gq)
	if !reflect.DeepEqual(gotG, wantG) {
		t.Fatalf("Neighbors = %+v, want %+v", gotG, wantG)
	}
}

func TestExport(t *testing.T) {
	f := newFake()
	c, _ := start(t, f.Driver())
	ref0 := direct(t, f.Driver())
	ctx := context.Background()
	h := open(t, c, &pb.ConnConfig{}).GetConn()

	q := driver.Query{Text: "select * from tobira_people order by id"}
	stream, err := c.Export(ctx, &pb.ExportRequest{Conn: h, Query: query(t, q), Format: "csv"})
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(chunk.GetData()) == 0 {
			t.Fatal("empty export chunk")
		}
		got.Write(chunk.GetData())
	}
	var want bytes.Buffer
	if err := ref0.(driver.Exporter).Export(ctx, q, "csv", &want); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want.Bytes()) || got.Len() == 0 {
		t.Fatalf("export differs:\n%s\nwant\n%s", got.String(), want.String())
	}

	bad, err := c.Export(ctx, &pb.ExportRequest{Conn: h, Query: query(t, q), Format: "xml"})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "unknown export format", drain(bad.Recv), codes.Unimplemented, "unsupported")
}

func TestWatch(t *testing.T) {
	f := newFake()
	c, _ := start(t, f.Driver())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := open(t, c, &pb.ConnConfig{}).GetConn()

	stream, err := c.Watch(ctx, convert.WatchRequestToProto(h, driver.WatchRequest{Collection: &people}))
	if err != nil {
		t.Fatal(err)
	}
	// Headers arrive once the driver's Watch is established.
	if _, err := stream.Header(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Insert(ctx, &pb.InsertRequest{Conn: h, Ref: ref(people), Values: obj(t, driver.Object{
		{Key: "id", Value: driver.Int(2000)}, {Key: "name", Value: driver.String("W")}, {Key: "email", Value: driver.String("w@example.com")},
	})}); err != nil {
		t.Fatal(err)
	}
	pe, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	ev, err := convert.EventFromProto(pe)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Op != "insert" {
		t.Fatalf("event %+v", ev)
	}
	if id, _ := ev.Key.Get("id"); !sameValue(id, driver.Int(2000)) {
		t.Fatalf("event key %v", ev.Key)
	}

	cancel()
	if err := drain(stream.Recv); err == nil {
		t.Fatal("a cancelled watch must end with an error")
	}
}

func TestWatchWithoutTarget(t *testing.T) {
	c, _ := start(t, newFake().Driver())
	h := open(t, c, &pb.ConnConfig{}).GetConn()
	stream, err := c.Watch(context.Background(), &pb.WatchRequest{Conn: h})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "watch nothing", drain(stream.Recv), codes.InvalidArgument, "syntax")
}

func TestTransactions(t *testing.T) {
	f := newFake()
	c, _ := start(t, f.Driver())
	ctx := context.Background()
	h := open(t, c, &pb.ConnConfig{}).GetConn()

	begin, err := c.Begin(ctx, &pb.BeginRequest{Conn: h, Options: convert.TxOptionsToProto(driver.TxOptions{Isolation: "serializable"})})
	if err != nil {
		t.Fatal(err)
	}
	tx := begin.GetTx()
	if tx.GetId() == "" || !slices.Equal(begin.GetCapabilities(), []string{"mutator", "explainer"}) {
		t.Fatalf("Begin = %v", begin)
	}

	ins, err := c.TxMutate(ctx, &pb.TxMutateRequest{Tx: tx, Op: &pb.TxMutateRequest_Insert{Insert: &pb.InsertOp{Ref: ref(people), Values: obj(t, driver.Object{
		{Key: "id", Value: driver.Int(3000)}, {Key: "name", Value: driver.String("T")}, {Key: "email", Value: driver.String("t@example.com")},
	})}}})
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := mustObject(t, ins.GetKey()).Get("id"); !sameValue(id, driver.Int(3000)) {
		t.Fatalf("tx insert key %v", ins.GetKey())
	}
	if _, err := c.TxMutate(ctx, &pb.TxMutateRequest{Tx: tx, Op: &pb.TxMutateRequest_Update{Update: &pb.UpdateOp{Ref: ref(people), Key: ins.GetKey(),
		Changes: obj(t, driver.Object{{Key: "name", Value: driver.String("T2")}})}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.TxMutate(ctx, &pb.TxMutateRequest{Tx: tx, Op: &pb.TxMutateRequest_Delete{Delete: &pb.DeleteOp{Ref: ref(people),
		Key: obj(t, driver.Object{{Key: "id", Value: driver.Int(5)}})}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.TxMutate(ctx, &pb.TxMutateRequest{Tx: tx}); err == nil {
		t.Fatal("a TxMutate without an op must fail")
	} else {
		wantStatus(t, "empty op", err, codes.InvalidArgument, "")
	}

	// The tx sees its own writes; the connection does not, until Commit.
	stream, err := c.TxQuery(ctx, &pb.TxQueryRequest{Tx: tx, Query: &pb.Query{Text: "select * from tobira_people where id = 3000"}})
	if err != nil {
		t.Fatal(err)
	}
	if rows := collect(t, stream.Recv).rows; len(rows) != 1 || !sameValue(rows[0][1], driver.String("T2")) {
		t.Fatalf("tx view %v", rows)
	}
	if rows := queryRows(t, c, h, "select * from tobira_people where id = 3000"); len(rows) != 0 {
		t.Fatalf("uncommitted write visible: %v", rows)
	}
	if _, err := c.TxExplain(ctx, &pb.TxExplainRequest{Tx: tx, Query: &pb.Query{Text: "select * from tobira_people"}}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Commit(ctx, &pb.CommitRequest{Tx: tx}); err != nil {
		t.Fatal(err)
	}
	if rows := queryRows(t, c, h, "select * from tobira_people where id = 3000"); len(rows) != 1 {
		t.Fatalf("committed write missing: %v", rows)
	}
	// Rollback after Commit is a no-op; the finished tx's handle is gone.
	if _, err := c.Rollback(ctx, &pb.RollbackRequest{Tx: tx}); err != nil {
		t.Fatalf("Rollback after Commit: %v", err)
	}
	_, err = c.TxQuery(ctx, &pb.TxQueryRequest{Tx: tx, Query: &pb.Query{Text: "select * from tobira_people"}})
	if err == nil {
		s, _ := c.TxQuery(ctx, &pb.TxQueryRequest{Tx: tx, Query: &pb.Query{Text: "select * from tobira_people"}})
		err = drain(s.Recv)
	}
	wantStatus(t, "query on a committed tx", err, codes.NotFound, "not_found")

	// Rollback discards.
	b2, err := c.Begin(ctx, &pb.BeginRequest{Conn: h})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.TxMutate(ctx, &pb.TxMutateRequest{Tx: b2.GetTx(), Op: &pb.TxMutateRequest_Delete{Delete: &pb.DeleteOp{Ref: ref(people),
		Key: obj(t, driver.Object{{Key: "id", Value: driver.Int(6)}})}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Rollback(ctx, &pb.RollbackRequest{Tx: b2.GetTx()}); err != nil {
		t.Fatal(err)
	}
	if rows := queryRows(t, c, h, "select * from tobira_people where id = 6"); len(rows) != 1 {
		t.Fatalf("rolled-back delete applied: %v", rows)
	}
	_, err = c.Commit(ctx, &pb.CommitRequest{Tx: b2.GetTx()})
	wantStatus(t, "commit after rollback", err, codes.NotFound, "not_found")
}

func mustObject(t *testing.T, p *pb.Object) driver.Object {
	t.Helper()
	o, err := convert.ObjectFromProto(p)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestCloseDropsTheConnAndItsTxs(t *testing.T) {
	f := newFake()
	c, _ := start(t, f.Driver())
	ctx := context.Background()
	h := open(t, c, &pb.ConnConfig{}).GetConn()
	b, err := c.Begin(ctx, &pb.BeginRequest{Conn: h})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Close(ctx, &pb.CloseRequest{Conn: h}); err != nil {
		t.Fatal(err)
	}
	_, err = c.Ping(ctx, &pb.PingRequest{Conn: h})
	wantStatus(t, "ping a closed conn", err, codes.NotFound, "not_found")
	_, err = c.Commit(ctx, &pb.CommitRequest{Tx: b.GetTx()})
	wantStatus(t, "commit a tx of a closed conn", err, codes.NotFound, "not_found")
	if !hasCall(f, "rollback") {
		t.Fatal("closing a conn must roll back its open txs")
	}
	// Closing again, or closing a handle that never existed, succeeds.
	if _, err := c.Close(ctx, &pb.CloseRequest{Conn: h}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Close(ctx, &pb.CloseRequest{Conn: &pb.ConnHandle{Id: "nope"}}); err != nil {
		t.Fatal(err)
	}
}

func hasCall(f *drivertest.Fake, op string) bool {
	for _, c := range f.Calls() {
		if c.Op == op {
			return true
		}
	}
	return false
}

func TestServerCloseReleasesEverything(t *testing.T) {
	d := &recordingDriver{Driver: newFake().Driver()}
	c, srv := start(t, d)
	ctx := context.Background()
	h1 := open(t, c, &pb.ConnConfig{}).GetConn()
	open(t, c, &pb.ConnConfig{})
	if _, err := c.Begin(ctx, &pb.BeginRequest{Conn: h1}); err != nil {
		t.Fatal(err)
	}
	if err := srv.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if n := d.closed.Load(); n != 2 {
		t.Fatalf("%d conns closed, want 2", n)
	}
	if d.rolledBack.Load() != 1 {
		t.Fatalf("%d txs rolled back, want 1", d.rolledBack.Load())
	}
	_, err := c.Ping(ctx, &pb.PingRequest{Conn: h1})
	wantStatus(t, "ping after shutdown", err, codes.NotFound, "not_found")
	_, err = c.Open(ctx, &pb.OpenRequest{Config: &pb.ConnConfig{}})
	wantStatus(t, "open after shutdown", err, codes.Unavailable, "connection")
	if err := srv.Shutdown(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestOpenFailsCleanly(t *testing.T) {
	f := newFake()
	f.FailNext("open", driver.Wrap(driver.ErrAuth, "fake", "open", errContaining("password authentication failed for hunter22")))
	c, _ := start(t, f.Driver())
	_, err := c.Open(context.Background(), &pb.OpenRequest{Config: &pb.ConnConfig{Dsn: "fake://u:hunter22@h/db"}})
	wantStatus(t, "open", err, codes.Unauthenticated, "auth")
	assertNoSecret(t, err, "hunter22")
}

func TestStreamsAfterTimeout(t *testing.T) {
	f := newFake()
	c, _ := start(t, f.Driver())
	h := open(t, c, &pb.ConnConfig{}).GetConn()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	stream, err := c.Query(ctx, &pb.QueryRequest{Conn: h, Query: &pb.Query{Text: "sleep 10s"}})
	if err == nil {
		err = drain(stream.Recv)
	}
	wantCode(t, err, codes.DeadlineExceeded)
	eventually(t, "the driver to see the deadline", func() bool {
		for _, call := range f.Calls() {
			if call.Op == "query" && call.Query.Text == "sleep 10s" && call.Err != nil {
				return true
			}
		}
		return false
	})
}

func TestServerOptionsCarryTheMessageCap(t *testing.T) {
	srv := sdk.NewServer(newFake().Driver(), sdk.WithLogger(slog.New(slog.DiscardHandler)))
	defer func() { _ = srv.Shutdown() }()
	if got, min := len(srv.ServerOptions()), len(convert.GRPCServerOptions())+2; got < min {
		t.Fatalf("%d server options, want the caps plus two interceptors", got)
	}
}

func TestCloseRollsBackEvenWhenCancelled(t *testing.T) {
	f := newFake()
	c, srv := start(t, f.Driver())
	h := open(t, c, &pb.ConnConfig{}).GetConn()
	if _, err := c.Begin(context.Background(), &pb.BeginRequest{Conn: h}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := srv.Close(ctx, &pb.CloseRequest{Conn: h}); err != nil {
		t.Fatalf("Close with a cancelled ctx: %v", err)
	}
	for _, call := range f.Calls() {
		if call.Op == "rollback" && call.Err != nil {
			t.Fatalf("rollback failed: %v", call.Err)
		}
	}
	if !hasCall(f, "rollback") {
		t.Fatal("no rollback")
	}
}

// nilWatchDriver's Conns return a nil channel and no error from Watch.
type nilWatchDriver struct{ driver.Driver }

type nilWatchConn struct{ driver.Conn }

func (nilWatchConn) Watch(context.Context, driver.WatchRequest) (<-chan driver.Event, error) {
	return nil, nil
}

func (d nilWatchDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	c, err := d.Driver.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return nilWatchConn{c}, nil
}

func TestWatchRejectsANilChannel(t *testing.T) {
	c, _ := start(t, nilWatchDriver{newFake().Driver()})
	h := open(t, c, &pb.ConnConfig{}).GetConn()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := c.Watch(ctx, &pb.WatchRequest{Conn: h, Channel: "x"})
	if err == nil {
		err = drain(s.Recv)
	}
	if code := status.Code(err); code == codes.DeadlineExceeded || err == nil {
		t.Fatalf("a nil watch channel must fail at once, got %v", err)
	}
	wantStatus(t, "nil channel", err, codes.Unknown, "")
}

// slowCommitDriver's Txs block in Commit until released, and count the
// Commit and Rollback calls that reach them.
type slowCommitDriver struct {
	driver.Driver
	entered, release   chan struct{}
	commits, rollbacks atomic.Int32
}

type slowCommitConn struct {
	driver.Conn
	d *slowCommitDriver
}

type slowCommitTx struct {
	driver.Tx
	d *slowCommitDriver
}

func (d *slowCommitDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	c, err := d.Driver.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return slowCommitConn{Conn: c, d: d}, nil
}

func (c slowCommitConn) Begin(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.Transactor).Begin(ctx, opts)
	if err != nil {
		return nil, err
	}
	return slowCommitTx{Tx: tx, d: c.d}, nil
}

func (t slowCommitTx) Commit(ctx context.Context) error {
	t.d.commits.Add(1)
	t.d.entered <- struct{}{}
	<-t.d.release
	return t.Tx.Commit(ctx)
}

func (t slowCommitTx) Rollback(ctx context.Context) error {
	t.d.rollbacks.Add(1)
	return t.Tx.Rollback(ctx)
}

func TestCommitAndRollbackNeverOverlap(t *testing.T) {
	d := &slowCommitDriver{Driver: newFake().Driver(), entered: make(chan struct{}), release: make(chan struct{})}
	c, _ := start(t, d)
	ctx := context.Background()
	h := open(t, c, &pb.ConnConfig{}).GetConn()
	b, err := c.Begin(ctx, &pb.BeginRequest{Conn: h})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.Commit(ctx, &pb.CommitRequest{Tx: b.GetTx()})
		done <- err
	}()
	<-d.entered

	// While Commit runs: a Rollback is refused with a conflict (the Tx may
	// still commit, so OK would be a lie) and never reaches the driver,
	// and a second Commit is refused.
	_, err = c.Rollback(ctx, &pb.RollbackRequest{Tx: b.GetTx()})
	wantStatus(t, "Rollback during Commit", err, codes.Aborted, "conflict")
	_, err = c.Commit(ctx, &pb.CommitRequest{Tx: b.GetTx()})
	wantStatus(t, "second Commit", err, codes.Aborted, "conflict")
	if d.rollbacks.Load() != 0 || d.commits.Load() != 1 {
		t.Fatalf("driver saw %d commits, %d rollbacks during the first Commit", d.commits.Load(), d.rollbacks.Load())
	}

	close(d.release)
	if err := <-done; err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := c.Commit(ctx, &pb.CommitRequest{Tx: b.GetTx()}); !errors.Is(convert.FromStatus(err), driver.ErrNotFound) {
		t.Fatalf("Commit after Commit: %v", err)
	}
}
