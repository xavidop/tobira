package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync/atomic"

	"google.golang.org/grpc"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/plugin/convert"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
)

// conn is the host side of a plugin Conn. It implements every capability
// interface statically and reports the ones the plugin advertised for this
// connection through driver.Supporter, so driver.As and CapabilitiesOf see
// exactly those; a call for any other fails with driver.ErrUnsupported
// without reaching the plugin.
type conn struct {
	a       *adapter
	p       *proc // the process this Conn lives in; it never moves
	h       *pb.ConnHandle
	cfg     driver.ConnConfig // for redaction and the embedder; never sent again
	caps    []driver.Capability
	formats []string
	closed  atomic.Bool
}

// invalidatorConn is a conn whose plugin implements CatalogInvalidator,
// which driver.As finds by plain type assertion.
type invalidatorConn struct{ *conn }

var (
	_ driver.Conn               = (*conn)(nil)
	_ driver.Supporter          = (*conn)(nil)
	_ driver.Browser            = (*conn)(nil)
	_ driver.Mutator            = (*conn)(nil)
	_ driver.Explainer          = (*conn)(nil)
	_ driver.Transactor         = (*conn)(nil)
	_ driver.VectorSearcher     = (*conn)(nil)
	_ driver.TimeSeriesReader   = (*conn)(nil)
	_ driver.GraphReader        = (*conn)(nil)
	_ driver.Completer          = (*conn)(nil)
	_ driver.Exporter           = (*conn)(nil)
	_ driver.Watcher            = (*conn)(nil)
	_ driver.CatalogInvalidator = invalidatorConn{}
)

// Supports implements driver.Supporter.
func (c *conn) Supports(cp driver.Capability) bool { return slices.Contains(c.caps, cp) }

// ready fails when the Conn is closed or its process is gone.
func (c *conn) ready(op string) error {
	if c.closed.Load() {
		return driver.Wrap(driver.ErrConnection, c.a.plugin.Name, op, errors.New("connection is closed"))
	}
	if !c.p.alive() {
		return c.a.lost(op)
	}
	return nil
}

// need is ready plus the capability check.
func (c *conn) need(op string, cp driver.Capability) error {
	if err := c.ready(op); err != nil {
		return err
	}
	if !c.Supports(cp) {
		return driver.Wrap(driver.ErrUnsupported, c.a.plugin.Name, op,
			fmt.Errorf("this connection does not offer %s", cp))
	}
	return nil
}

func (c *conn) fail(ctx context.Context, op string, err error) error {
	return c.a.callError(ctx, c.p, op, err, c.cfg)
}

// decodeErr wraps a host-side decode failure (ErrLimit, ErrInvalid). A
// value or frame over a limit is classified as driver.ErrLimit.
func (c *conn) decodeErr(op string, err error) error {
	err = fmt.Errorf("plugin %q: %s: %w", c.a.plugin.Name, op, err)
	if errors.Is(err, convert.ErrLimit) {
		err = &driver.Error{Kind: driver.ErrLimit, Err: err}
	}
	return redactErr(err, c.cfg)
}

// Ping implements driver.Conn.
func (c *conn) Ping(ctx context.Context) error {
	if err := c.ready("ping"); err != nil {
		return err
	}
	_, err := c.p.rpc.Ping(ctx, &pb.PingRequest{Conn: c.h})
	return c.fail(ctx, "ping", err)
}

// Close implements driver.Conn. It is idempotent. Closing a Conn whose
// process is gone succeeds: there is nothing left to release.
func (c *conn) Close() error {
	if c.closed.Swap(true) || !c.p.alive() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	_, err := c.p.rpc.Close(ctx, &pb.CloseRequest{Conn: c.h})
	if err = c.fail(ctx, "close", err); errors.Is(err, driver.ErrConnection) && !c.p.alive() {
		return nil
	}
	return err
}

// Namespaces implements driver.Conn.
func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	if err := c.ready("namespaces"); err != nil {
		return nil, err
	}
	resp, err := c.p.rpc.Namespaces(ctx, &pb.NamespacesRequest{Conn: c.h})
	if err != nil {
		return nil, c.fail(ctx, "namespaces", err)
	}
	nss, err := convert.NamespacesFromProto(resp.GetNamespaces())
	if err != nil {
		return nil, c.decodeErr("namespaces", err)
	}
	return nss, nil
}

// Collections implements driver.Conn.
func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	if err := c.ready("collections"); err != nil {
		return nil, err
	}
	resp, err := c.p.rpc.Collections(ctx, &pb.CollectionsRequest{Conn: c.h, Namespace: ns.Name})
	if err != nil {
		return nil, c.fail(ctx, "collections", err)
	}
	cs, err := convert.CollectionsFromProto(resp.GetCollections())
	if err != nil {
		return nil, c.decodeErr("collections", err)
	}
	return cs, nil
}

// Describe implements driver.Conn.
func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	if err := c.ready("describe"); err != nil {
		return nil, err
	}
	resp, err := c.p.rpc.Describe(ctx, &pb.DescribeRequest{Conn: c.h, Ref: convert.CollectionRefToProto(ref)})
	if err != nil {
		return nil, c.fail(ctx, "describe", err)
	}
	s, err := convert.SchemaFromProto(resp.GetSchema())
	if err != nil {
		return nil, c.decodeErr("describe", err)
	}
	return s, nil
}

// frames opens a result stream with call and reads it as a ResultSet. The
// stream lives on a child of ctx, like a database/sql cursor: it ends when
// ctx does, when the ResultSet is closed or exhausted, or when a Next's
// own ctx ends while it waits.
func (c *conn) frames(ctx context.Context, op string,
	call func(context.Context) (grpc.ServerStreamingClient[pb.ResultFrame], error),
) (driver.ResultSet, error) {
	sctx, cancel := context.WithCancel(ctx)
	stream, err := call(sctx)
	if err != nil {
		cancel()
		return nil, c.fail(sctx, op, err)
	}
	recv := func() (*pb.ResultFrame, error) {
		f, err := stream.Recv()
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, c.fail(sctx, op, err)
		}
		return f, err
	}
	rs, err := convert.NewFrameResultSet(sctx, recv, cancel)
	if err != nil {
		return nil, c.streamErr(op, err)
	}
	return &resultSet{MultiResultSet: rs, c: c, op: op}, nil
}

// streamErr leaves a mapped driver or context error as it is and wraps a
// decode failure.
func (c *conn) streamErr(op string, err error) error {
	if errors.Is(err, convert.ErrLimit) || errors.Is(err, convert.ErrInvalid) {
		return c.decodeErr(op, err)
	}
	return err
}

// resultSet wraps the frame reader so decode failures name the plugin.
type resultSet struct {
	driver.MultiResultSet
	c  *conn
	op string
}

func (r *resultSet) Next(ctx context.Context) (driver.Row, bool, error) {
	row, ok, err := r.MultiResultSet.Next(ctx)
	if err != nil {
		err = r.c.streamErr(r.op, err)
	}
	return row, ok, err
}

func (r *resultSet) NextResultSet(ctx context.Context) (bool, error) {
	ok, err := r.MultiResultSet.NextResultSet(ctx)
	if err != nil {
		err = r.c.streamErr(r.op, err)
	}
	return ok, err
}

// Query implements driver.Querier.
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	if err := c.ready("query"); err != nil {
		return nil, err
	}
	pq, err := convert.QueryToProto(q)
	if err != nil {
		return nil, c.decodeErr("query", err)
	}
	return c.frames(ctx, "query", func(sctx context.Context) (grpc.ServerStreamingClient[pb.ResultFrame], error) {
		return c.p.rpc.Query(sctx, &pb.QueryRequest{Conn: c.h, Query: pq})
	})
}

// Browse implements driver.Browser.
func (c *conn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	if err := c.need("browse", driver.CapBrowser); err != nil {
		return nil, err
	}
	po, err := convert.BrowseOptionsToProto(opts)
	if err != nil {
		return nil, c.decodeErr("browse", err)
	}
	pref := convert.CollectionRefToProto(ref)
	return c.frames(ctx, "browse", func(sctx context.Context) (grpc.ServerStreamingClient[pb.ResultFrame], error) {
		return c.p.rpc.Browse(sctx, &pb.BrowseRequest{Conn: c.h, Ref: pref, Options: po})
	})
}

// Insert implements driver.Mutator.
func (c *conn) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	if err := c.need("insert", driver.CapMutator); err != nil {
		return nil, err
	}
	pv, err := convert.ObjectToProto(values)
	if err != nil {
		return nil, c.decodeErr("insert", err)
	}
	resp, err := c.p.rpc.Insert(ctx, &pb.InsertRequest{Conn: c.h, Ref: convert.CollectionRefToProto(ref), Values: pv})
	if err != nil {
		return nil, c.fail(ctx, "insert", err)
	}
	key, err := convert.ObjectFromProto(resp.GetKey())
	if err != nil {
		return nil, c.decodeErr("insert", err)
	}
	return key, nil
}

// Update implements driver.Mutator.
func (c *conn) Update(ctx context.Context, ref driver.CollectionRef, key, changes driver.Object) error {
	if err := c.need("update", driver.CapMutator); err != nil {
		return err
	}
	pk, err := convert.ObjectToProto(key)
	if err != nil {
		return c.decodeErr("update", err)
	}
	pc, err := convert.ObjectToProto(changes)
	if err != nil {
		return c.decodeErr("update", err)
	}
	_, err = c.p.rpc.Update(ctx, &pb.UpdateRequest{Conn: c.h, Ref: convert.CollectionRefToProto(ref), Key: pk, Changes: pc})
	return c.fail(ctx, "update", err)
}

// Delete implements driver.Mutator.
func (c *conn) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	if err := c.need("delete", driver.CapMutator); err != nil {
		return err
	}
	pk, err := convert.ObjectToProto(key)
	if err != nil {
		return c.decodeErr("delete", err)
	}
	_, err = c.p.rpc.Delete(ctx, &pb.DeleteRequest{Conn: c.h, Ref: convert.CollectionRefToProto(ref), Key: pk})
	return c.fail(ctx, "delete", err)
}

// Explain implements driver.Explainer.
func (c *conn) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	if err := c.need("explain", driver.CapExplainer); err != nil {
		return nil, err
	}
	pq, err := convert.QueryToProto(q)
	if err != nil {
		return nil, c.decodeErr("explain", err)
	}
	resp, err := c.p.rpc.Explain(ctx, &pb.ExplainRequest{Conn: c.h, Query: pq, Options: convert.ExplainOptionsToProto(opts)})
	if err != nil {
		return nil, c.fail(ctx, "explain", err)
	}
	plan, err := convert.PlanFromProto(resp.GetPlan())
	if err != nil {
		return nil, c.decodeErr("explain", err)
	}
	return plan, nil
}

// Begin implements driver.Transactor.
func (c *conn) Begin(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := c.need("begin", driver.CapTransactor); err != nil {
		return nil, err
	}
	resp, err := c.p.rpc.Begin(ctx, &pb.BeginRequest{Conn: c.h, Options: convert.TxOptionsToProto(opts)})
	if err != nil {
		return nil, c.fail(ctx, "begin", err)
	}
	h := resp.GetTx()
	if h == nil || h.GetId() == "" || len(h.GetId()) > convert.MaxNameBytes {
		return nil, c.decodeErr("begin", fmt.Errorf("%w: bad transaction handle", convert.ErrInvalid))
	}
	// A Tx offers no capability its Conn lacks.
	return &tx{c: c, h: &pb.TxHandle{Id: h.GetId()}, caps: intersect(capsFromStrings(resp.GetCapabilities()), c.caps)}, nil
}

// Complete implements driver.Completer.
func (c *conn) Complete(ctx context.Context, req driver.CompletionRequest) ([]driver.Completion, error) {
	if err := c.need("complete", driver.CapCompleter); err != nil {
		return nil, err
	}
	resp, err := c.p.rpc.Complete(ctx, convert.CompleteRequestToProto(c.h, req))
	if err != nil {
		return nil, c.fail(ctx, "complete", err)
	}
	cs, err := convert.CompletionsFromProto(resp.GetCompletions())
	if err != nil {
		return nil, c.decodeErr("complete", err)
	}
	return cs, nil
}

// Invalidate implements driver.CatalogInvalidator. It takes no context,
// so the RPC is bounded by closeTimeout; a failure only means the plugin's
// cache stays as it was.
func (c invalidatorConn) Invalidate() {
	if c.ready("invalidate") != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	_, _ = c.p.rpc.Invalidate(ctx, &pb.InvalidateRequest{Conn: c.h})
}
