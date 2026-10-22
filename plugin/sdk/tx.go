package sdk

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/plugin/convert"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
)

// Begin implements pb.DriverServer.
func (s *Server) Begin(ctx context.Context, req *pb.BeginRequest) (*pb.BeginResponse, error) {
	e, t, err := connCapability[driver.Transactor](s, req.GetConn(), driver.CapTransactor)
	if err != nil {
		return nil, err
	}
	tx, err := t.Begin(ctx, convert.TxOptionsFromProto(req.GetOptions()))
	if err != nil {
		return nil, e.fail(err)
	}
	if tx == nil {
		return nil, e.fail(errNoTx)
	}
	id := newID()
	s.mu.Lock()
	if s.conns[e.id] != e {
		// The Conn was closed while Begin ran.
		s.mu.Unlock()
		rollbackOrphan(ctx, tx, e.cfg)
		return nil, errUnknownConn()
	}
	s.txs[id] = &txEntry{tx: tx, conn: e}
	e.txs[id] = struct{}{}
	s.mu.Unlock()
	return &pb.BeginResponse{Tx: &pb.TxHandle{Id: id}, Capabilities: capStrings(driver.CapabilitiesOf(tx))}, nil
}

// rollbackOrphan rolls back a Tx whose Conn was closed while Begin ran. The
// Conn is no longer live, so a panic is redacted with its config here.
func rollbackOrphan(ctx context.Context, tx driver.Tx, cfg driver.ConnConfig) {
	defer repanicRedacted(cfg)
	_ = tx.Rollback(ctx)
}

// dropTx forgets a Tx handle.
func (s *Server) dropTx(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.txs[id]; ok {
		delete(t.conn.txs, id)
		delete(s.txs, id)
	}
}

// TxQuery implements pb.DriverServer.
func (s *Server) TxQuery(req *pb.TxQueryRequest, stream grpc.ServerStreamingServer[pb.ResultFrame]) error {
	t, err := s.tx(req.GetTx())
	if err != nil {
		return err
	}
	q, err := convert.QueryFromProto(req.GetQuery())
	if err != nil {
		return t.conn.fail(err)
	}
	rs, err := t.tx.Query(stream.Context(), q)
	return sendResult(stream, t.conn.fail, rs, err)
}

// TxMutate implements pb.DriverServer.
func (s *Server) TxMutate(ctx context.Context, req *pb.TxMutateRequest) (*pb.TxMutateResponse, error) {
	t, err := s.tx(req.GetTx())
	if err != nil {
		return nil, err
	}
	m, ok := driver.As[driver.Mutator](t.tx)
	if !ok {
		return nil, s.unsupported(driver.CapMutator, "transaction")
	}
	switch op := req.GetOp().(type) {
	case *pb.TxMutateRequest_Insert:
		key, err := insert(ctx, m, op.Insert.GetRef(), op.Insert.GetValues())
		if err != nil {
			return nil, t.conn.fail(err)
		}
		return &pb.TxMutateResponse{Key: key}, nil
	case *pb.TxMutateRequest_Update:
		if err := update(ctx, m, op.Update.GetRef(), op.Update.GetKey(), op.Update.GetChanges()); err != nil {
			return nil, t.conn.fail(err)
		}
	case *pb.TxMutateRequest_Delete:
		if err := remove(ctx, m, op.Delete.GetRef(), op.Delete.GetKey()); err != nil {
			return nil, t.conn.fail(err)
		}
	default:
		return nil, t.conn.fail(errNoOp)
	}
	return &pb.TxMutateResponse{}, nil
}

// TxExplain implements pb.DriverServer.
func (s *Server) TxExplain(ctx context.Context, req *pb.TxExplainRequest) (*pb.ExplainResponse, error) {
	t, err := s.tx(req.GetTx())
	if err != nil {
		return nil, err
	}
	x, ok := driver.As[driver.Explainer](t.tx)
	if !ok {
		return nil, s.unsupported(driver.CapExplainer, "transaction")
	}
	plan, err := explain(ctx, x, req.GetQuery(), req.GetOptions())
	if err != nil {
		return nil, t.conn.fail(err)
	}
	return &pb.ExplainResponse{Plan: plan}, nil
}

// Commit implements pb.DriverServer. A committed Tx's handle is released;
// after a failed Commit it stays, so the host can still Rollback. While a
// Commit runs, a second Commit and a Rollback both fail with
// driver.ErrConflict, so the driver's Tx never sees both at once.
func (s *Server) Commit(ctx context.Context, req *pb.CommitRequest) (*pb.CommitResponse, error) {
	id := req.GetTx().GetId()
	s.mu.Lock()
	t, ok := s.txs[id]
	switch {
	case !ok:
		s.mu.Unlock()
		return nil, errUnknownTx()
	case t.committing:
		s.mu.Unlock()
		return nil, errCommitting()
	}
	t.committing = true
	s.mu.Unlock()

	if err := t.tx.Commit(ctx); err != nil {
		s.mu.Lock()
		t.committing = false
		s.mu.Unlock()
		return nil, t.conn.fail(err)
	}
	s.dropTx(id)
	return &pb.CommitResponse{}, nil
}

// Rollback implements pb.DriverServer. It releases the handle, and succeeds
// for an unknown one: Rollback after Commit is a no-op. A Rollback while a
// Commit runs fails with driver.ErrConflict and never reaches the driver:
// that Commit may still succeed, so success would claim a discard that
// did not happen. If the Commit fails, the handle stays for another
// Rollback (or the Conn's Close).
func (s *Server) Rollback(ctx context.Context, req *pb.RollbackRequest) (*pb.RollbackResponse, error) {
	id := req.GetTx().GetId()
	s.mu.Lock()
	t, ok := s.txs[id]
	switch {
	case !ok:
		s.mu.Unlock()
		return &pb.RollbackResponse{}, nil
	case t.committing:
		s.mu.Unlock()
		return nil, errCommitting()
	}
	delete(t.conn.txs, id)
	delete(s.txs, id)
	s.mu.Unlock()
	if err := t.tx.Rollback(ctx); err != nil {
		return nil, t.conn.fail(err)
	}
	return &pb.RollbackResponse{}, nil
}

func errCommitting() error {
	return convert.StatusError(codes.Aborted, driver.ErrConflict, "transaction is already committing")
}
