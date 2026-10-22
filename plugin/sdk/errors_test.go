package sdk_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/plugin/convert"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
)

// connRPCs calls every RPC that takes a ConnHandle, returning each one's
// error (for a stream, the first Recv's).
func connRPCs(t *testing.T, c pb.DriverClient, h *pb.ConnHandle) map[string]error {
	t.Helper()
	ctx := context.Background()
	q := &pb.Query{Text: "select * from tobira_people"}
	call := func(_ any, err error) error { return err }
	stream := func(recv func() error, err error) error {
		if err != nil {
			return err
		}
		return recv()
	}
	return map[string]error{
		"Ping":        call(c.Ping(ctx, &pb.PingRequest{Conn: h})),
		"Namespaces":  call(c.Namespaces(ctx, &pb.NamespacesRequest{Conn: h})),
		"Collections": call(c.Collections(ctx, &pb.CollectionsRequest{Conn: h})),
		"Describe":    call(c.Describe(ctx, &pb.DescribeRequest{Conn: h, Ref: ref(people)})),
		"Insert":      call(c.Insert(ctx, &pb.InsertRequest{Conn: h, Ref: ref(people)})),
		"Update":      call(c.Update(ctx, &pb.UpdateRequest{Conn: h, Ref: ref(people)})),
		"Delete":      call(c.Delete(ctx, &pb.DeleteRequest{Conn: h, Ref: ref(people)})),
		"Explain":     call(c.Explain(ctx, &pb.ExplainRequest{Conn: h, Query: q})),
		"Begin":       call(c.Begin(ctx, &pb.BeginRequest{Conn: h})),
		"Complete":    call(c.Complete(ctx, &pb.CompleteRequest{Conn: h})),
		"Invalidate":  call(c.Invalidate(ctx, &pb.InvalidateRequest{Conn: h})),
		"Neighbors":   call(c.Neighbors(ctx, &pb.NeighborsRequest{Conn: h, Query: &pb.GraphQuery{}})),
		"Query": func() error {
			s, err := c.Query(ctx, &pb.QueryRequest{Conn: h, Query: q})
			return stream(func() error { _, err := s.Recv(); return err }, err)
		}(),
		"Browse": func() error {
			s, err := c.Browse(ctx, &pb.BrowseRequest{Conn: h, Ref: ref(people)})
			return stream(func() error { _, err := s.Recv(); return err }, err)
		}(),
		"Similar": func() error {
			s, err := c.Similar(ctx, &pb.SimilarRequest{Conn: h, Ref: ref(vectors), Query: &pb.VectorQuery{Vector: []float32{1}}})
			return stream(func() error { _, err := s.Recv(); return err }, err)
		}(),
		"Range": func() error {
			s, err := c.Range(ctx, &pb.RangeRequest{Conn: h, Ref: ref(cpu), Query: &pb.RangeQuery{}})
			return stream(func() error { _, err := s.Recv(); return err }, err)
		}(),
		"Export": func() error {
			s, err := c.Export(ctx, &pb.ExportRequest{Conn: h, Query: q, Format: "csv"})
			return stream(func() error { _, err := s.Recv(); return err }, err)
		}(),
		"Watch": func() error {
			// A watch that is established streams until cancelled, so a
			// short deadline that expires means it worked.
			wctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
			defer cancel()
			s, err := c.Watch(wctx, &pb.WatchRequest{Conn: h, Channel: "x"})
			err = stream(func() error { _, err := s.Recv(); return err }, err)
			if status.Code(err) == codes.DeadlineExceeded {
				return nil
			}
			return err
		}(),
	}
}

// txRPCs calls every RPC that takes a TxHandle except Rollback.
func txRPCs(t *testing.T, c pb.DriverClient, tx *pb.TxHandle) map[string]error {
	t.Helper()
	ctx := context.Background()
	call := func(_ any, err error) error { return err }
	return map[string]error{
		"TxQuery": func() error {
			s, err := c.TxQuery(ctx, &pb.TxQueryRequest{Tx: tx, Query: &pb.Query{Text: "select * from tobira_people"}})
			if err != nil {
				return err
			}
			_, err = s.Recv()
			return err
		}(),
		"TxMutate":  call(c.TxMutate(ctx, &pb.TxMutateRequest{Tx: tx, Op: &pb.TxMutateRequest_Delete{Delete: &pb.DeleteOp{Ref: ref(people)}}})),
		"TxExplain": call(c.TxExplain(ctx, &pb.TxExplainRequest{Tx: tx, Query: &pb.Query{Text: "x"}})),
		"Commit":    call(c.Commit(ctx, &pb.CommitRequest{Tx: tx})),
	}
}

func TestUnknownHandlesAreNotFound(t *testing.T) {
	c, _ := start(t, newFake().Driver())
	open(t, c, &pb.ConnConfig{}) // a live conn must not be reachable by a guessed id
	for name, err := range connRPCs(t, c, &pb.ConnHandle{Id: "no-such-conn"}) {
		wantStatus(t, name, err, codes.NotFound, "not_found")
	}
	for name, err := range connRPCs(t, c, nil) {
		wantStatus(t, name+" (no handle)", err, codes.NotFound, "not_found")
	}
	for name, err := range txRPCs(t, c, &pb.TxHandle{Id: "no-such-tx"}) {
		wantStatus(t, name, err, codes.NotFound, "not_found")
	}
	// Release calls on unknown handles succeed: they are idempotent.
	if _, err := c.Rollback(context.Background(), &pb.RollbackRequest{Tx: &pb.TxHandle{Id: "no-such-tx"}}); err != nil {
		t.Fatalf("Rollback of an unknown tx: %v", err)
	}
}

func TestAConnHandleIsNotATxHandle(t *testing.T) {
	c, _ := start(t, newFake().Driver())
	h := open(t, c, &pb.ConnConfig{}).GetConn()
	for name, err := range txRPCs(t, c, &pb.TxHandle{Id: h.GetId()}) {
		wantStatus(t, name, err, codes.NotFound, "not_found")
	}
}

func TestUnimplementedCapabilities(t *testing.T) {
	c, _ := start(t, bareDriver{newFake().Driver()})
	resp := open(t, c, &pb.ConnConfig{})
	if len(resp.GetCapabilities()) != 0 || resp.GetCatalogInvalidator() || len(resp.GetExportFormats()) != 0 {
		t.Fatalf("a bare conn advertised %v", resp)
	}
	core := map[string]bool{"Ping": true, "Namespaces": true, "Collections": true, "Describe": true, "Query": true}
	for name, err := range connRPCs(t, c, resp.GetConn()) {
		if core[name] {
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			continue
		}
		wantStatus(t, name, err, codes.Unimplemented, "unsupported")
	}
}

func TestDisabledCapabilitiesAreNotAdvertised(t *testing.T) {
	f := newFake(drivertest.WithoutCapabilities(driver.CapBrowser, driver.CapMutator, driver.CapExporter))
	c, _ := start(t, f.Driver())
	resp := open(t, c, &pb.ConnConfig{})
	for _, cap := range resp.GetCapabilities() {
		if cap == "browser" || cap == "mutator" || cap == "exporter" {
			t.Fatalf("advertised disabled capability %q", cap)
		}
	}
	if len(resp.GetExportFormats()) != 0 {
		t.Fatal("export formats advertised without the exporter capability")
	}
	errs := connRPCs(t, c, resp.GetConn())
	for _, name := range []string{"Browse", "Insert", "Update", "Delete", "Export"} {
		wantStatus(t, name, errs[name], codes.Unimplemented, "unsupported")
	}
	if hasCall(f, "browse") || hasCall(f, "insert") || hasCall(f, "export") {
		t.Fatal("an unadvertised capability reached the driver")
	}

	// A Tx without the mutator capability refuses TxMutate.
	b, err := c.Begin(context.Background(), &pb.BeginRequest{Conn: resp.GetConn()})
	if err != nil {
		t.Fatal(err)
	}
	for _, cap := range b.GetCapabilities() {
		if cap == "mutator" {
			t.Fatal("tx advertised the disabled mutator")
		}
	}
	_, err = c.TxMutate(context.Background(), &pb.TxMutateRequest{Tx: b.GetTx(), Op: &pb.TxMutateRequest_Delete{Delete: &pb.DeleteOp{Ref: ref(people)}}})
	wantStatus(t, "TxMutate", err, codes.Unimplemented, "unsupported")
}

func TestDriverErrorsMapToStatus(t *testing.T) {
	cases := []struct {
		kind error
		code codes.Code
	}{
		{driver.ErrSyntax, codes.InvalidArgument},
		{driver.ErrNotFound, codes.NotFound},
		{driver.ErrReadOnly, codes.FailedPrecondition},
		{driver.ErrConflict, codes.Aborted},
		{driver.ErrPermission, codes.PermissionDenied},
		{driver.ErrConnection, codes.Unavailable},
	}
	for _, tc := range cases {
		t.Run(driver.KindName(tc.kind), func(t *testing.T) {
			f := newFake()
			in := &driver.Error{Kind: tc.kind, Driver: "fake", Op: "query", Code: "E1", Err: errors.New("it broke")}
			f.FailNext("query", in)
			c, _ := start(t, f.Driver())
			h := open(t, c, &pb.ConnConfig{}).GetConn()
			s, err := c.Query(context.Background(), &pb.QueryRequest{Conn: h, Query: &pb.Query{Text: "select * from tobira_people"}})
			if err == nil {
				_, err = s.Recv()
			}
			wantStatus(t, "query", err, tc.code, driver.KindName(tc.kind))
			back := convert.FromStatus(err)
			if !errors.Is(back, tc.kind) || back.Error() != in.Error() {
				t.Fatalf("host sees %q, want %q", back, in)
			}
		})
	}
}

func TestErrorMessagesAreRedacted(t *testing.T) {
	f := newFake()
	const dsnPassword, password = "hunter22", "t0p-s3cret"
	leak := errContaining("auth failed: fake://u:" + dsnPassword + "@h/db, password=" + password + " and " + password)
	f.FailNext("ping", driver.Wrap(driver.ErrAuth, "fake", "ping", leak))
	f.FailNext("query", driver.Wrap(driver.ErrSyntax, "fake", "query", leak))
	c, _ := start(t, f.Driver())
	h := open(t, c, &pb.ConnConfig{Dsn: "fake://u:" + dsnPassword + "@h/db", Password: password}).GetConn()

	_, err := c.Ping(context.Background(), &pb.PingRequest{Conn: h})
	wantStatus(t, "ping", err, codes.Unauthenticated, "auth")
	assertNoSecret(t, err, dsnPassword)
	assertNoSecret(t, err, password)

	s, err := c.Query(context.Background(), &pb.QueryRequest{Conn: h, Query: &pb.Query{Text: "x"}})
	if err == nil {
		_, err = s.Recv()
	}
	wantStatus(t, "query", err, codes.InvalidArgument, "syntax")
	assertNoSecret(t, err, dsnPassword)
	assertNoSecret(t, err, password)
}

func TestInvalidRequestsAreRejected(t *testing.T) {
	f := newFake()
	c, _ := start(t, f.Driver())
	h := open(t, c, &pb.ConnConfig{}).GetConn()
	// A parameter with no kind set is malformed.
	s, err := c.Query(context.Background(), &pb.QueryRequest{Conn: h, Query: &pb.Query{Text: "select * from tobira_people", Params: []*pb.Value{{}}}})
	if err == nil {
		_, err = s.Recv()
	}
	wantStatus(t, "bad param", err, codes.InvalidArgument, "")
	if hasCall(f, "query") {
		t.Fatal("a malformed request reached the driver")
	}
}
