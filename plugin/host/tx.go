package host

import (
	"context"
	"fmt"
	"slices"

	"google.golang.org/grpc"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/plugin/convert"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
)

// tx is the host side of a plugin Tx. Like conn, it implements Mutator and
// Explainer statically and reports through driver.Supporter the ones the
// plugin's Tx offers.
type tx struct {
	c    *conn
	h    *pb.TxHandle
	caps []driver.Capability
}

var (
	_ driver.Tx        = (*tx)(nil)
	_ driver.Supporter = (*tx)(nil)
	_ driver.Mutator   = (*tx)(nil)
	_ driver.Explainer = (*tx)(nil)
)

// Supports implements driver.Supporter.
func (t *tx) Supports(cp driver.Capability) bool { return slices.Contains(t.caps, cp) }

func (t *tx) need(op string, cp driver.Capability) error {
	if err := t.c.ready(op); err != nil {
		return err
	}
	if !t.Supports(cp) {
		return driver.Wrap(driver.ErrUnsupported, t.c.a.plugin.Name, op,
			fmt.Errorf("this transaction does not offer %s", cp))
	}
	return nil
}

// Query implements driver.Querier.
func (t *tx) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	if err := t.c.ready("query"); err != nil {
		return nil, err
	}
	pq, err := convert.QueryToProto(q)
	if err != nil {
		return nil, t.c.decodeErr("query", err)
	}
	return t.c.frames(ctx, "query", func(sctx context.Context) (grpc.ServerStreamingClient[pb.ResultFrame], error) {
		return t.c.p.rpc.TxQuery(sctx, &pb.TxQueryRequest{Tx: t.h, Query: pq})
	})
}

// Commit implements driver.Tx.
func (t *tx) Commit(ctx context.Context) error {
	if err := t.c.ready("commit"); err != nil {
		return err
	}
	_, err := t.c.p.rpc.Commit(ctx, &pb.CommitRequest{Tx: t.h})
	return t.c.fail(ctx, "commit", err)
}

// Rollback implements driver.Tx. On a dead process there is nothing left
// to roll back, and it fails with driver.ErrConnection.
func (t *tx) Rollback(ctx context.Context) error {
	if err := t.c.ready("rollback"); err != nil {
		return err
	}
	_, err := t.c.p.rpc.Rollback(ctx, &pb.RollbackRequest{Tx: t.h})
	return t.c.fail(ctx, "rollback", err)
}

func (t *tx) mutate(ctx context.Context, op string, req *pb.TxMutateRequest) (*pb.TxMutateResponse, error) {
	req.Tx = t.h
	resp, err := t.c.p.rpc.TxMutate(ctx, req)
	if err != nil {
		return nil, t.c.fail(ctx, op, err)
	}
	return resp, nil
}

// Insert implements driver.Mutator.
func (t *tx) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	if err := t.need("insert", driver.CapMutator); err != nil {
		return nil, err
	}
	pv, err := convert.ObjectToProto(values)
	if err != nil {
		return nil, t.c.decodeErr("insert", err)
	}
	resp, err := t.mutate(ctx, "insert", &pb.TxMutateRequest{Op: &pb.TxMutateRequest_Insert{
		Insert: &pb.InsertOp{Ref: convert.CollectionRefToProto(ref), Values: pv},
	}})
	if err != nil {
		return nil, err
	}
	key, err := convert.ObjectFromProto(resp.GetKey())
	if err != nil {
		return nil, t.c.decodeErr("insert", err)
	}
	return key, nil
}

// Update implements driver.Mutator.
func (t *tx) Update(ctx context.Context, ref driver.CollectionRef, key, changes driver.Object) error {
	if err := t.need("update", driver.CapMutator); err != nil {
		return err
	}
	pk, err := convert.ObjectToProto(key)
	if err != nil {
		return t.c.decodeErr("update", err)
	}
	pc, err := convert.ObjectToProto(changes)
	if err != nil {
		return t.c.decodeErr("update", err)
	}
	_, err = t.mutate(ctx, "update", &pb.TxMutateRequest{Op: &pb.TxMutateRequest_Update{
		Update: &pb.UpdateOp{Ref: convert.CollectionRefToProto(ref), Key: pk, Changes: pc},
	}})
	return err
}

// Delete implements driver.Mutator.
func (t *tx) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	if err := t.need("delete", driver.CapMutator); err != nil {
		return err
	}
	pk, err := convert.ObjectToProto(key)
	if err != nil {
		return t.c.decodeErr("delete", err)
	}
	_, err = t.mutate(ctx, "delete", &pb.TxMutateRequest{Op: &pb.TxMutateRequest_Delete{
		Delete: &pb.DeleteOp{Ref: convert.CollectionRefToProto(ref), Key: pk},
	}})
	return err
}

// Explain implements driver.Explainer.
func (t *tx) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	if err := t.need("explain", driver.CapExplainer); err != nil {
		return nil, err
	}
	pq, err := convert.QueryToProto(q)
	if err != nil {
		return nil, t.c.decodeErr("explain", err)
	}
	resp, err := t.c.p.rpc.TxExplain(ctx, &pb.TxExplainRequest{Tx: t.h, Query: pq, Options: convert.ExplainOptionsToProto(opts)})
	if err != nil {
		return nil, t.c.fail(ctx, "explain", err)
	}
	plan, err := convert.PlanFromProto(resp.GetPlan())
	if err != nil {
		return nil, t.c.decodeErr("explain", err)
	}
	return plan, nil
}
