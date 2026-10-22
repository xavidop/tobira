package sdk_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"math"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"sync"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/plugin/convert"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
	"github.com/xavidop/tobira/plugin/sdk"
)

// start serves d through an sdk.Server over an in-process gRPC connection,
// with the same message caps the real plugin uses, and returns a raw
// protocol client.
func start(t *testing.T, d driver.Driver) (pb.DriverClient, *sdk.Server) {
	t.Helper()
	return startWith(t, d, sdk.WithLogger(slog.New(slog.DiscardHandler)))
}

// startWith is start with the server's options; the gRPC server gets
// srv.ServerOptions(), as Serve uses.
func startWith(t *testing.T, d driver.Driver, opts ...sdk.Option) (pb.DriverClient, *sdk.Server) {
	t.Helper()
	srv := sdk.NewServer(d, opts...)
	gs := grpc.NewServer(srv.ServerOptions()...)
	pb.RegisterDriverServer(gs, srv)
	lis := bufconn.Listen(1 << 20)
	go func() { _ = gs.Serve(lis) }()
	cc, err := grpc.NewClient("passthrough:///bufnet", append(convert.GRPCDialOptions(),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cc.Close()
		gs.Stop()
		_ = srv.Shutdown()
	})
	return pb.NewDriverClient(cc), srv
}

var (
	people  = driver.CollectionRef{Namespace: "main", Name: "tobira_people"}
	vectors = driver.CollectionRef{Namespace: "main", Name: drivertest.VectorCollection}
	cpu     = driver.CollectionRef{Namespace: "main", Name: drivertest.TimeSeriesCollection}
)

// newFake is a Fake with every fixture, plus a two-node graph.
func newFake(opts ...drivertest.FakeOption) *drivertest.Fake {
	f := drivertest.NewFake(append([]drivertest.FakeOption{
		drivertest.WithPeople(), drivertest.WithVectors(), drivertest.WithTimeSeries(),
	}, opts...)...)
	f.AddCollection("main", "nodes",
		[]driver.Column{{Name: "id", Type: driver.TypeInt}, {Name: "label", Type: driver.TypeString}},
		[]driver.Row{{driver.Int(1), driver.String("Person")}, {driver.Int(2), driver.String("Person")}}, "id")
	f.AddCollection("main", "edges",
		[]driver.Column{{Name: "id", Type: driver.TypeInt}, {Name: "from", Type: driver.TypeInt}, {Name: "to", Type: driver.TypeInt}, {Name: "type", Type: driver.TypeString}},
		[]driver.Row{{driver.Int(1), driver.Int(1), driver.Int(2), driver.String("KNOWS")}}, "id")
	return f
}

// direct opens a Conn on d in-process, the reference the RPC results are
// compared with.
func direct(t *testing.T, d driver.Driver) driver.Conn {
	t.Helper()
	c, err := d.Open(context.Background(), driver.ConnConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func open(t *testing.T, c pb.DriverClient, cfg *pb.ConnConfig) *pb.OpenResponse {
	t.Helper()
	resp, err := c.Open(context.Background(), &pb.OpenRequest{Config: cfg})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if resp.GetConn().GetId() == "" {
		t.Fatal("Open returned an empty handle")
	}
	return resp
}

// result is a decoded result set.
type result struct {
	cols  []driver.Column
	rows  []driver.Row
	stats driver.Stats
}

// collect decodes a ResultFrame stream with the host-side reader.
func collect(t *testing.T, recv func() (*pb.ResultFrame, error)) result {
	t.Helper()
	rs, err := convert.NewFrameResultSet(context.Background(), recv, func() {})
	if err != nil {
		t.Fatalf("first frame: %v", err)
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(context.Background(), rs, 0)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	return result{cols, rows, st}
}

// collectDirect drains a ResultSet the driver returned in-process.
func collectDirect(t *testing.T, call func() (driver.ResultSet, error)) result {
	t.Helper()
	rs, err := call()
	if err != nil {
		t.Fatal(err)
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(context.Background(), rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	return result{cols, rows, st}
}

func sameResult(t *testing.T, what string, got, want result) {
	t.Helper()
	if len(got.cols) != len(want.cols) {
		t.Fatalf("%s: %d columns, want %d", what, len(got.cols), len(want.cols))
	}
	for i := range got.cols {
		g, w := got.cols[i], want.cols[i]
		if g.Name != w.Name || g.Type != w.Type || g.NativeType != w.NativeType || g.Key != w.Key ||
			(g.Source == nil) != (w.Source == nil) || (g.Source != nil && *g.Source != *w.Source) {
			t.Fatalf("%s: column %d = %+v, want %+v", what, i, g, w)
		}
	}
	sameRows(t, what, got.rows, want.rows)
	gs, ws := got.stats, want.stats
	gs.Duration, ws.Duration = 0, 0
	if gs.RowsAffected != ws.RowsAffected || gs.Truncated != ws.Truncated || gs.NextCursor != ws.NextCursor ||
		len(gs.Warnings) != len(ws.Warnings) || len(gs.Notices) != len(ws.Notices) {
		t.Fatalf("%s: stats %+v, want %+v", what, gs, ws)
	}
}

func sameRows(t *testing.T, what string, got, want []driver.Row) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d rows, want %d", what, len(got), len(want))
	}
	for i := range got {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("%s: row %d has %d values, want %d", what, i, len(got[i]), len(want[i]))
		}
		for j := range got[i] {
			if !sameValue(got[i][j], want[i][j]) {
				t.Fatalf("%s: row %d value %d = %v (%v), want %v (%v)", what, i, j, got[i][j], got[i][j].Type(), want[i][j], want[i][j].Type())
			}
		}
	}
}

// sameValue is exact equality: same type, same float bits, same bytes, same
// members in order, and times with the same instant, zone name and offset.
func sameValue(a, b driver.Value) bool {
	if a.Type() != b.Type() {
		return false
	}
	switch a.Type() {
	case driver.TypeNull:
		return true
	case driver.TypeBool:
		x, _ := a.AsBool()
		y, _ := b.AsBool()
		return x == y
	case driver.TypeInt:
		x, _ := a.AsInt()
		y, _ := b.AsInt()
		return x == y
	case driver.TypeFloat:
		x, _ := a.AsFloat()
		y, _ := b.AsFloat()
		return math.Float64bits(x) == math.Float64bits(y)
	case driver.TypeDecimal, driver.TypeString:
		x, _ := a.AsText()
		y, _ := b.AsText()
		return x == y
	case driver.TypeBytes:
		x, _ := a.AsBytes()
		y, _ := b.AsBytes()
		return bytes.Equal(x, y)
	case driver.TypeTime:
		x, _ := a.AsTime()
		y, _ := b.AsTime()
		xn, xo := x.Zone()
		yn, yo := y.Zone()
		return x.Equal(y) && xn == yn && xo == yo
	case driver.TypeJSON:
		x, _ := a.AsJSON()
		y, _ := b.AsJSON()
		return bytes.Equal(x, y)
	case driver.TypeVector:
		x, _ := a.AsVector()
		y, _ := b.AsVector()
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if math.Float32bits(x[i]) != math.Float32bits(y[i]) {
				return false
			}
		}
		return true
	case driver.TypeList:
		x, _ := a.AsList()
		y, _ := b.AsList()
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if !sameValue(x[i], y[i]) {
				return false
			}
		}
		return true
	case driver.TypeObject:
		x, _ := a.AsObject()
		y, _ := b.AsObject()
		return sameObject(x, y)
	case driver.TypeUnknown:
		xn, xr, _ := a.AsUnknown()
		yn, yr, _ := b.AsUnknown()
		return xn == yn && bytes.Equal(xr, yr) && a.String() == b.String()
	}
	return false
}

func sameObject(x, y driver.Object) bool {
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i].Key != y[i].Key || !sameValue(x[i].Value, y[i].Value) {
			return false
		}
	}
	return true
}

// wantStatus asserts err is a gRPC status with code and an ErrorInfo of
// kind (a driver kind name, "" for unclassified).
func wantStatus(t *testing.T, what string, err error, code codes.Code, kind string) {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok || err == nil {
		t.Fatalf("%s: %v, want a %v status", what, err, code)
	}
	if st.Code() != code {
		t.Fatalf("%s: code %v (%s), want %v", what, st.Code(), st.Message(), code)
	}
	info := convert.ErrorInfoOf(st)
	if info == nil {
		t.Fatalf("%s: status has no ErrorInfo", what)
	}
	if info.GetKind() != kind {
		t.Fatalf("%s: kind %q, want %q", what, info.GetKind(), kind)
	}
}

// drain reads a stream to its end, returning the terminal error (nil at EOF).
func drain[T any](recv func() (T, error)) error {
	for {
		if _, err := recv(); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func ref(r driver.CollectionRef) *pb.CollectionRef { return convert.CollectionRefToProto(r) }

func obj(t *testing.T, o driver.Object) *pb.Object {
	t.Helper()
	p, err := convert.ObjectToProto(o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func query(t *testing.T, q driver.Query) *pb.Query {
	t.Helper()
	p, err := convert.QueryToProto(q)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
