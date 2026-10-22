// Command plugin is the driver plugin plugin/host's tests launch. It serves
// the drivertest Fake (seeded with People) through plugin/sdk, plus a few
// scripted queries, or misbehaves on purpose: TOBIRA_PLUGIN_TEST_MODE
// (allow-listed, like every TOBIRA_PLUGIN_ variable) picks the behaviour.
//
// Modes:
//
//	""        the Fake, plus the queries "!env" (one row per environment
//	          entry), "!crash" (one frame of rows, then blocks until the
//	          process dies), "!exit" (the process exits at once, so a
//	          "!crash" stream the host has started reading dies mid-stream),
//	          "!hang" (one frame of rows, then blocks until cancelled) and
//	          "!stdin" (what reading stdin gives within a second) and
//	          "!wide" (a narrow row, a row of about 18 MiB, too wide for
//	          one gRPC message, then a narrow row)
//	cookie    expects another host's magic cookie, so it refuses to start
//	proto2    speaks protocol version 2
//	stall     never completes the handshake
//	hostile   sends oversized and malformed answers, bypassing the SDK's
//	          caps, and leaks the Open credentials in Ping's error
//	bad-info  answers Info with a name over the host's limit
//	bad-info-once   answers the first Info over the host's limit, then
//	                normally; "!closes" counts the Close RPCs received
//	slow-info-once  stalls the first Info until cancelled, then answers
//	                normally; "!closes" likewise
//	sneaky    a Fake without mutator whose Open and Begin still claim it;
//	          "!rpcs" lists the mutating RPCs that reached the plugin
//
// In every mode, TOBIRA_PLUGIN_TEST_STARTS names a file the plugin appends
// a line to as it starts, so tests can count starts; in the default mode,
// TOBIRA_PLUGIN_TEST_KIND replaces the Kind its Info reports.
package main

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/plugin/convert"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
	"github.com/xavidop/tobira/plugin/sdk"
	"github.com/xavidop/tobira/plugin/shared"
)

func main() {
	if path := os.Getenv("TOBIRA_PLUGIN_TEST_STARTS"); path != "" {
		recordStart(path)
	}
	d := newDriver().Driver()
	if kind := os.Getenv("TOBIRA_PLUGIN_TEST_KIND"); kind != "" {
		d = kinded{Driver: d, kind: driver.Kind(kind)}
	}
	switch os.Getenv("TOBIRA_PLUGIN_TEST_MODE") {
	case "cookie":
		hs := shared.Handshake()
		hs.MagicCookieKey, hs.MagicCookieValue = "SOME_OTHER_HOST_COOKIE", "nope"
		serveRaw(hs, sdk.NewServer(d))
	case "proto2":
		hs := shared.Handshake()
		hs.ProtocolVersion = 2
		serveRaw(hs, sdk.NewServer(d))
	case "stall":
		time.Sleep(time.Hour)
	case "hostile":
		serveRaw(shared.Handshake(), &hostile{Server: sdk.NewServer(d)})
	case "sneaky":
		sn := &sneaky{}
		f := newDriver(drivertest.WithoutCapabilities(driver.CapMutator))
		f.OnQuery("!rpcs", func(driver.Query) (driver.ResultSet, error) {
			sn.mu.Lock()
			defer sn.mu.Unlock()
			rows := []driver.Row{}
			for _, r := range sn.rpcs {
				rows = append(rows, driver.Row{driver.String(r)})
			}
			return driver.NewSliceResultSet(textCol, rows, driver.Stats{RowsAffected: -1}), nil
		})
		sn.Server = sdk.NewServer(f.Driver())
		serveRaw(shared.Handshake(), sn)
	case "bad-info-once", "slow-info-once":
		fl := &flakyInfo{slow: os.Getenv("TOBIRA_PLUGIN_TEST_MODE") == "slow-info-once"}
		f := newDriver()
		f.OnQuery("!closes", func(driver.Query) (driver.ResultSet, error) {
			n := strconv.Itoa(int(fl.closes.Load()))
			return driver.NewSliceResultSet(textCol, []driver.Row{{driver.String(n)}}, driver.Stats{RowsAffected: -1}), nil
		})
		fl.Server = sdk.NewServer(f.Driver())
		serveRaw(shared.Handshake(), fl)
	case "bad-info":
		serveRaw(shared.Handshake(), &badInfo{Server: sdk.NewServer(d)})
	default:
		sdk.Serve(d)
	}
}

func serveRaw(hs plugin.HandshakeConfig, srv pb.DriverServer) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: hs,
		Plugins:         shared.PluginSet(srv),
		GRPCServer: func(opts []grpc.ServerOption) *grpc.Server {
			return grpc.NewServer(append(opts, convert.GRPCServerOptions()...)...)
		},
	})
}

// recordStart appends a line to the file at path.
func recordStart(path string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(2)
	}
	_, _ = io.WriteString(f, "start\n")
	_ = f.Close()
}

// kinded is a driver whose Info reports another Kind.
type kinded struct {
	driver.Driver
	kind driver.Kind
}

func (k kinded) Info() driver.Info {
	i := k.Driver.Info()
	i.Kind = k.kind
	return i
}

var textCol = []driver.Column{{Name: "v", Type: driver.TypeString, NativeType: "text"}}

// newDriver is the Fake with the "!" queries scripted on top.
func newDriver(opts ...drivertest.FakeOption) *drivertest.Fake {
	f := drivertest.NewFake(append([]drivertest.FakeOption{drivertest.WithPeople(), drivertest.WithVectors()}, opts...)...)
	f.OnQuery("!env", func(driver.Query) (driver.ResultSet, error) {
		var rows []driver.Row
		for _, kv := range os.Environ() {
			rows = append(rows, driver.Row{driver.String(kv)})
		}
		return driver.NewSliceResultSet(textCol, rows, driver.Stats{RowsAffected: -1}), nil
	})
	// "!crash" blocks after one frame instead of exiting on its own: had it
	// exited after N rows, it could die before gRPC flushed its first frame,
	// and the host's Query (which reads the columns frame) would fail
	// instead of the stream. The host sends "!exit" once it has read rows.
	f.OnQuery("!crash", func(driver.Query) (driver.ResultSet, error) { return &endless{hangAt: convert.RowsPerFrame}, nil })
	f.OnQuery("!exit", func(driver.Query) (driver.ResultSet, error) {
		os.Exit(3)
		return nil, nil
	})
	f.OnQuery("!wide", func(driver.Query) (driver.ResultSet, error) {
		cols := make([]driver.Column, 20)
		narrow, wide := make(driver.Row, 20), make(driver.Row, 20)
		for i := range cols {
			cols[i] = driver.Column{Name: "c" + strconv.Itoa(i), Type: driver.TypeString, NativeType: "text"}
			narrow[i], wide[i] = driver.String("a"), driver.String(strings.Repeat("x", 900<<10))
		}
		return driver.NewSliceResultSet(cols, []driver.Row{narrow, wide, narrow}, driver.Stats{RowsAffected: -1}), nil
	})
	f.OnQuery("!hang", func(driver.Query) (driver.ResultSet, error) { return &endless{hangAt: convert.RowsPerFrame}, nil })
	f.OnQuery("!stdin", func(driver.Query) (driver.ResultSet, error) {
		return driver.NewSliceResultSet(textCol, []driver.Row{{driver.String(readStdin())}}, driver.Stats{RowsAffected: -1}), nil
	})
	return f
}

// readStdin reports what stdin yields within a second: "eof:<data>" or
// "blocked:<data>".
func readStdin() string {
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(os.Stdin)
		done <- "eof:" + string(b)
	}()
	select {
	case s := <-done:
		return s
	case <-time.After(time.Second):
		return "blocked:"
	}
}

// endless yields rows until hangAt, then Next blocks until its ctx ends
// (or the process dies).
type endless struct {
	n, hangAt int
}

func (e *endless) Columns() []driver.Column { return textCol }
func (e *endless) Stats() driver.Stats      { return driver.Stats{} }
func (e *endless) Close() error             { return nil }

func (e *endless) Next(ctx context.Context) (driver.Row, bool, error) {
	e.n++
	if e.hangAt > 0 && e.n > e.hangAt {
		<-ctx.Done()
		return nil, false, ctx.Err()
	}
	return driver.Row{driver.String(strings.Repeat("x", 8))}, true, nil
}

// hostile answers with what the SDK would never send.
type hostile struct {
	*sdk.Server
	mu     sync.Mutex
	secret string // the last Open's DSN and password, leaked by Ping
}

func (h *hostile) Query(req *pb.QueryRequest, stream grpc.ServerStreamingServer[pb.ResultFrame]) error {
	switch req.GetQuery().GetText() {
	case "huge value":
		if err := stream.Send(&pb.ResultFrame{Frame: &pb.ResultFrame_Columns{Columns: &pb.Columns{
			Columns: []*pb.Column{{Name: "v", Type: pb.ValueType_VALUE_TYPE_STRING}},
		}}}); err != nil {
			return err
		}
		big := &pb.Value{Kind: &pb.Value_StringValue{StringValue: strings.Repeat("a", 2<<20)}}
		return stream.Send(&pb.ResultFrame{Frame: &pb.ResultFrame_Rows{Rows: &pb.RowBatch{
			Rows: []*pb.Row{{Values: []*pb.Value{big}}},
		}}})
	case "too many columns":
		cols := make([]*pb.Column, convert.MaxColumns+1)
		for i := range cols {
			cols[i] = &pb.Column{Name: "c", Type: pb.ValueType_VALUE_TYPE_INT}
		}
		return stream.Send(&pb.ResultFrame{Frame: &pb.ResultFrame_Columns{Columns: &pb.Columns{Columns: cols}}})
	case "over message cap":
		big := &pb.Column{Name: strings.Repeat("n", convert.MaxMessageBytes+1)}
		return stream.Send(&pb.ResultFrame{Frame: &pb.ResultFrame_Columns{Columns: &pb.Columns{Columns: []*pb.Column{big}}}})
	}
	return h.Server.Query(req, stream)
}

func (h *hostile) Open(ctx context.Context, req *pb.OpenRequest) (*pb.OpenResponse, error) {
	h.mu.Lock()
	h.secret = req.GetConfig().GetDsn() + " password=" + req.GetConfig().GetPassword()
	h.mu.Unlock()
	resp, err := h.Server.Open(ctx, req)
	if err != nil {
		return nil, err
	}
	// Junk capabilities and formats must be ignored, not trusted.
	for range 10000 {
		resp.Capabilities = append(resp.Capabilities, strings.Repeat("z", 100))
	}
	return resp, nil
}

func (h *hostile) Namespaces(context.Context, *pb.NamespacesRequest) (*pb.NamespacesResponse, error) {
	return &pb.NamespacesResponse{Namespaces: []*pb.Namespace{{Name: strings.Repeat("n", convert.MaxNameBytes+1)}}}, nil
}

// Ping leaks the credentials unredacted; the host must mask them.
func (h *hostile) Ping(context.Context, *pb.PingRequest) (*pb.PingResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return nil, convert.StatusError(codes.Unauthenticated, driver.ErrAuth, "cannot log in to "+h.secret)
}

func (h *hostile) Collections(context.Context, *pb.CollectionsRequest) (*pb.CollectionsResponse, error) {
	return nil, convert.StatusError(codes.NotFound, driver.ErrNotFound, strings.Repeat("m", 1<<20))
}

// badInfo answers Info with a name the host refuses.
type badInfo struct{ *sdk.Server }

func (b *badInfo) Info(context.Context, *pb.InfoRequest) (*pb.InfoResponse, error) {
	return &pb.InfoResponse{Info: &pb.DriverInfo{Name: strings.Repeat("n", convert.MaxNameBytes+1), Kind: "sql"}}, nil
}

// sneaky claims mutator at Open and Begin though its Info does not, and
// records every mutating RPC the host sends anyway.
type sneaky struct {
	*sdk.Server
	mu   sync.Mutex
	rpcs []string
}

func (s *sneaky) record(name string) {
	s.mu.Lock()
	s.rpcs = append(s.rpcs, name)
	s.mu.Unlock()
}

func (s *sneaky) Open(ctx context.Context, req *pb.OpenRequest) (*pb.OpenResponse, error) {
	resp, err := s.Server.Open(ctx, req)
	if err == nil {
		resp.Capabilities = append(resp.Capabilities, string(driver.CapMutator))
	}
	return resp, err
}

func (s *sneaky) Begin(ctx context.Context, req *pb.BeginRequest) (*pb.BeginResponse, error) {
	resp, err := s.Server.Begin(ctx, req)
	if err == nil {
		resp.Capabilities = append(resp.Capabilities, string(driver.CapMutator))
	}
	return resp, err
}

func (s *sneaky) Insert(ctx context.Context, req *pb.InsertRequest) (*pb.InsertResponse, error) {
	s.record("insert")
	return s.Server.Insert(ctx, req)
}

func (s *sneaky) Update(ctx context.Context, req *pb.UpdateRequest) (*pb.UpdateResponse, error) {
	s.record("update")
	return s.Server.Update(ctx, req)
}

func (s *sneaky) Delete(ctx context.Context, req *pb.DeleteRequest) (*pb.DeleteResponse, error) {
	s.record("delete")
	return s.Server.Delete(ctx, req)
}

func (s *sneaky) TxMutate(ctx context.Context, req *pb.TxMutateRequest) (*pb.TxMutateResponse, error) {
	s.record("txmutate")
	return s.Server.TxMutate(ctx, req)
}

// flakyInfo fails (or stalls) its first Info and counts Close RPCs.
type flakyInfo struct {
	*sdk.Server
	slow   bool
	infos  atomic.Int32
	closes atomic.Int32
}

func (f *flakyInfo) Info(ctx context.Context, req *pb.InfoRequest) (*pb.InfoResponse, error) {
	if f.infos.Add(1) == 1 {
		if f.slow {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &pb.InfoResponse{Info: &pb.DriverInfo{Name: strings.Repeat("n", convert.MaxNameBytes+1), Kind: "sql"}}, nil
	}
	return f.Server.Info(ctx, req)
}

func (f *flakyInfo) Close(ctx context.Context, req *pb.CloseRequest) (*pb.CloseResponse, error) {
	f.closes.Add(1)
	return f.Server.Close(ctx, req)
}
