package sdk

import (
	"context"

	"google.golang.org/grpc"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/plugin/convert"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
)

// sendResult streams what a driver call returned: its error as the RPC's
// status, or the result set as frames under the stream's context, so a
// host cancel stops the driver mid-stream. The result set is always closed.
func sendResult(stream grpc.ServerStreamingServer[pb.ResultFrame], fail func(error) error, rs driver.ResultSet, err error) error {
	if err != nil {
		return fail(err)
	}
	if rs == nil {
		return fail(errNoResult)
	}
	return fail(convert.SendResultSet(stream.Context(), rs, stream.Send))
}

// Query implements pb.DriverServer.
func (s *Server) Query(req *pb.QueryRequest, stream grpc.ServerStreamingServer[pb.ResultFrame]) error {
	e, err := s.conn(req.GetConn())
	if err != nil {
		return err
	}
	q, err := convert.QueryFromProto(req.GetQuery())
	if err != nil {
		return e.fail(err)
	}
	rs, err := e.conn.Query(stream.Context(), q)
	return sendResult(stream, e.fail, rs, err)
}

// Browse implements pb.DriverServer.
func (s *Server) Browse(req *pb.BrowseRequest, stream grpc.ServerStreamingServer[pb.ResultFrame]) error {
	e, b, err := connCapability[driver.Browser](s, req.GetConn(), driver.CapBrowser)
	if err != nil {
		return err
	}
	ref, err := convert.CollectionRefFromProto(req.GetRef())
	if err != nil {
		return e.fail(err)
	}
	opts, err := convert.BrowseOptionsFromProto(req.GetOptions())
	if err != nil {
		return e.fail(err)
	}
	rs, err := b.Browse(stream.Context(), ref, opts)
	return sendResult(stream, e.fail, rs, err)
}

// Insert implements pb.DriverServer.
func (s *Server) Insert(ctx context.Context, req *pb.InsertRequest) (*pb.InsertResponse, error) {
	e, m, err := connCapability[driver.Mutator](s, req.GetConn(), driver.CapMutator)
	if err != nil {
		return nil, err
	}
	key, err := insert(ctx, m, req.GetRef(), req.GetValues())
	if err != nil {
		return nil, e.fail(err)
	}
	return &pb.InsertResponse{Key: key}, nil
}

// Update implements pb.DriverServer.
func (s *Server) Update(ctx context.Context, req *pb.UpdateRequest) (*pb.UpdateResponse, error) {
	e, m, err := connCapability[driver.Mutator](s, req.GetConn(), driver.CapMutator)
	if err != nil {
		return nil, err
	}
	if err := update(ctx, m, req.GetRef(), req.GetKey(), req.GetChanges()); err != nil {
		return nil, e.fail(err)
	}
	return &pb.UpdateResponse{}, nil
}

// Delete implements pb.DriverServer.
func (s *Server) Delete(ctx context.Context, req *pb.DeleteRequest) (*pb.DeleteResponse, error) {
	e, m, err := connCapability[driver.Mutator](s, req.GetConn(), driver.CapMutator)
	if err != nil {
		return nil, err
	}
	if err := remove(ctx, m, req.GetRef(), req.GetKey()); err != nil {
		return nil, e.fail(err)
	}
	return &pb.DeleteResponse{}, nil
}

// insert, update and remove decode a Mutator call and run it on m, for a
// Conn and a Tx alike.
func insert(ctx context.Context, m driver.Mutator, pref *pb.CollectionRef, pvalues *pb.Object) (*pb.Object, error) {
	ref, err := convert.CollectionRefFromProto(pref)
	if err != nil {
		return nil, err
	}
	values, err := convert.ObjectFromProto(pvalues)
	if err != nil {
		return nil, err
	}
	key, err := m.Insert(ctx, ref, values)
	if err != nil {
		return nil, err
	}
	return convert.ObjectToProto(key)
}

func update(ctx context.Context, m driver.Mutator, pref *pb.CollectionRef, pkey, pchanges *pb.Object) error {
	ref, err := convert.CollectionRefFromProto(pref)
	if err != nil {
		return err
	}
	key, err := convert.ObjectFromProto(pkey)
	if err != nil {
		return err
	}
	changes, err := convert.ObjectFromProto(pchanges)
	if err != nil {
		return err
	}
	return m.Update(ctx, ref, key, changes)
}

func remove(ctx context.Context, m driver.Mutator, pref *pb.CollectionRef, pkey *pb.Object) error {
	ref, err := convert.CollectionRefFromProto(pref)
	if err != nil {
		return err
	}
	key, err := convert.ObjectFromProto(pkey)
	if err != nil {
		return err
	}
	return m.Delete(ctx, ref, key)
}

// Explain implements pb.DriverServer.
func (s *Server) Explain(ctx context.Context, req *pb.ExplainRequest) (*pb.ExplainResponse, error) {
	e, x, err := connCapability[driver.Explainer](s, req.GetConn(), driver.CapExplainer)
	if err != nil {
		return nil, err
	}
	plan, err := explain(ctx, x, req.GetQuery(), req.GetOptions())
	if err != nil {
		return nil, e.fail(err)
	}
	return &pb.ExplainResponse{Plan: plan}, nil
}

// explain decodes an Explain call and runs it on x, for a Conn and a Tx.
func explain(ctx context.Context, x driver.Explainer, pq *pb.Query, popts *pb.ExplainOptions) (*pb.Plan, error) {
	q, err := convert.QueryFromProto(pq)
	if err != nil {
		return nil, err
	}
	plan, err := x.Explain(ctx, q, convert.ExplainOptionsFromProto(popts))
	if err != nil {
		return nil, err
	}
	return convert.PlanToProto(plan)
}

// Complete implements pb.DriverServer.
func (s *Server) Complete(ctx context.Context, req *pb.CompleteRequest) (*pb.CompleteResponse, error) {
	e, c, err := connCapability[driver.Completer](s, req.GetConn(), driver.CapCompleter)
	if err != nil {
		return nil, err
	}
	cs, err := c.Complete(ctx, convert.CompletionRequestFromProto(req))
	if err != nil {
		return nil, e.fail(err)
	}
	p, err := convert.CompletionsToProto(cs)
	if err != nil {
		return nil, e.fail(err)
	}
	return &pb.CompleteResponse{Completions: p}, nil
}

// Invalidate implements pb.DriverServer.
func (s *Server) Invalidate(_ context.Context, req *pb.InvalidateRequest) (*pb.InvalidateResponse, error) {
	e, err := s.conn(req.GetConn())
	if err != nil {
		return nil, err
	}
	inv, ok := driver.As[driver.CatalogInvalidator](e.conn)
	if !ok {
		return nil, s.unsupported("catalog invalidation", "connection")
	}
	inv.Invalidate()
	return &pb.InvalidateResponse{}, nil
}
