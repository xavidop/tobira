package sdk

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/plugin/convert"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
)

// shutdownTimeout bounds each rollback Shutdown runs on a Tx the host left open.
const shutdownTimeout = 5 * time.Second

// Server is the plugin side of the protocol: a pb.DriverServer that runs
// every call on a driver.Driver. Serve builds one; NewServer is for
// serving it on a gRPC server of your own, such as an in-process test of a
// driver over the real protocol. It is safe for concurrent use.
type Server struct {
	pb.UnimplementedDriverServer

	d   driver.Driver
	log *slog.Logger

	mu     sync.Mutex
	conns  map[string]*connEntry
	txs    map[string]*txEntry
	closed bool
}

// connEntry is a live Conn behind a handle.
type connEntry struct {
	id   string
	conn driver.Conn
	cfg  driver.ConnConfig
	txs  map[string]struct{} // ids of this Conn's open Txs
}

// txEntry is a live Tx behind a handle.
type txEntry struct {
	tx   driver.Tx
	conn *connEntry
	// committing is set (under Server.mu) while Commit runs, so a
	// concurrent Commit or Rollback never reaches the driver's Tx.
	committing bool
}

// Option configures NewServer.
type Option func(*Server)

// WithLogger sets the logger the server logs to and derives each
// ConnConfig.Logger from. The default writes JSON lines to stderr.
func WithLogger(l *slog.Logger) Option {
	return func(s *Server) {
		if l != nil {
			s.log = l
		}
	}
}

// NewServer returns a Server for d.
func NewServer(d driver.Driver, opts ...Option) *Server {
	s := &Server{d: d, conns: map[string]*connEntry{}, txs: map[string]*txEntry{}}
	for _, o := range opts {
		o(s)
	}
	if s.log == nil {
		s.log = newLogger(os.Stderr, slog.LevelInfo)
	}
	return s
}

// Shutdown rolls back every open Tx and closes every Conn, then refuses new
// Opens. It is idempotent and returns the first error a driver reported.
// (Close is the protocol's Close RPC.)
func (s *Server) Shutdown() error {
	s.mu.Lock()
	s.closed = true
	txs, conns := s.txs, s.conns
	s.txs, s.conns = map[string]*txEntry{}, map[string]*connEntry{}
	s.mu.Unlock()

	var errs []error
	for _, t := range txs {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		errs = append(errs, t.tx.Rollback(ctx))
		cancel()
	}
	for _, c := range conns {
		errs = append(errs, c.conn.Close())
	}
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// repanicRedacted, deferred, re-raises a panic with its message redacted
// with cfg. It is for driver calls made while cfg is not among the live
// configs the panic interceptor redacts with: before Open registers the
// Conn, and after Close (or a Begin that lost a race with Close) removed it.
func repanicRedacted(cfg driver.ConnConfig) {
	if r := recover(); r != nil {
		panic(driver.RedactMessage(fmt.Sprint(r), cfg))
	}
}

// newID returns an unguessable handle id.
func newID() string { return rand.Text() }

// redactor masks cfg's credentials in a message.
func redactor(cfg driver.ConnConfig) func(string) string {
	return func(s string) string { return driver.RedactMessage(s, cfg) }
}

// fail turns a driver (or decode) error into the RPC's status, redacted
// with the Conn's config. A nil error stays nil.
func (e *connEntry) fail(err error) error { return convert.ToStatus(err, redactor(e.cfg)) }

func errUnknownConn() error {
	return convert.StatusError(codes.NotFound, driver.ErrNotFound, "unknown connection handle")
}

func errUnknownTx() error {
	return convert.StatusError(codes.NotFound, driver.ErrNotFound, "unknown transaction handle")
}

// conn looks up a Conn by handle.
func (s *Server) conn(h *pb.ConnHandle) (*connEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.conns[h.GetId()]; ok {
		return e, nil
	}
	return nil, errUnknownConn()
}

// tx looks up a Tx by handle.
func (s *Server) tx(h *pb.TxHandle) (*txEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.txs[h.GetId()]; ok {
		return e, nil
	}
	return nil, errUnknownTx()
}

// unsupported is the status for a call whose capability the target lacks.
func (s *Server) unsupported(c driver.Capability, what string) error {
	return convert.StatusError(codes.Unimplemented, driver.ErrUnsupported,
		fmt.Sprintf("%s: this %s does not offer %s", s.d.Info().Name, what, c))
}

// capability returns e's Conn as T, or Unimplemented when driver.As says
// the Conn does not offer c.
func capability[T any](s *Server, e *connEntry, c driver.Capability) (T, error) {
	v, ok := driver.As[T](e.conn)
	if !ok {
		return v, s.unsupported(c, "connection")
	}
	return v, nil
}

// connCapability looks up a Conn and its capability T in one step.
func connCapability[T any](s *Server, h *pb.ConnHandle, c driver.Capability) (*connEntry, T, error) {
	e, err := s.conn(h)
	if err != nil {
		var zero T
		return nil, zero, err
	}
	v, err := capability[T](s, e, c)
	return e, v, err
}

func capStrings(cs []driver.Capability) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = string(c)
	}
	return out
}

// Info implements pb.DriverServer.
func (s *Server) Info(context.Context, *pb.InfoRequest) (*pb.InfoResponse, error) {
	info, err := convert.InfoToProto(s.d.Info())
	if err != nil {
		return nil, convert.ToStatus(err, nil)
	}
	return &pb.InfoResponse{Info: info}, nil
}

// Open implements pb.DriverServer.
func (s *Server) Open(ctx context.Context, req *pb.OpenRequest) (*pb.OpenResponse, error) {
	cfg := convert.ConnConfigFromProto(req.GetConfig())
	if s.isClosed() {
		return nil, errShuttingDown()
	}
	cfg.Logger = s.log.With("conn", cfg.Name)
	// This config is not live yet, so the panic interceptor cannot redact
	// with it: redact a panic in the driver's Open here and re-raise it.
	defer repanicRedacted(cfg)
	conn, err := s.d.Open(ctx, cfg)
	if err != nil {
		return nil, convert.ToStatus(err, redactor(cfg))
	}
	if conn == nil {
		return nil, convert.StatusError(codes.Internal, nil, s.d.Info().Name+": Open returned no connection")
	}
	e := &connEntry{id: newID(), conn: conn, cfg: cfg, txs: map[string]struct{}{}}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = conn.Close()
		return nil, errShuttingDown()
	}
	s.conns[e.id] = e
	s.mu.Unlock()

	resp := &pb.OpenResponse{Conn: &pb.ConnHandle{Id: e.id}, Capabilities: capStrings(driver.CapabilitiesOf(conn))}
	_, resp.CatalogInvalidator = driver.As[driver.CatalogInvalidator](conn)
	if ex, ok := driver.As[driver.Exporter](conn); ok {
		resp.ExportFormats = ex.ExportFormats()
	}
	return resp, nil
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func errShuttingDown() error {
	return convert.StatusError(codes.Unavailable, driver.ErrConnection, "plugin is shutting down")
}

// Close implements pb.DriverServer. It rolls back the Conn's open Txs and
// succeeds for an unknown handle.
func (s *Server) Close(ctx context.Context, req *pb.CloseRequest) (*pb.CloseResponse, error) {
	s.mu.Lock()
	e, ok := s.conns[req.GetConn().GetId()]
	var txs []driver.Tx
	if ok {
		delete(s.conns, e.id)
		for id := range e.txs {
			txs = append(txs, s.txs[id].tx)
			delete(s.txs, id)
		}
	}
	s.mu.Unlock()
	if !ok {
		return &pb.CloseResponse{}, nil
	}
	// The Conn is no longer live, so the panic interceptor cannot redact
	// with its config: a panic in the rollbacks or the Close is redacted
	// here.
	defer repanicRedacted(e.cfg)
	// The Conn is gone whatever happens to this call, so its rollbacks must
	// not fail just because the host cancelled Close.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	var errs []error
	for _, tx := range txs {
		errs = append(errs, tx.Rollback(rctx))
	}
	errs = append(errs, e.conn.Close())
	if err := errors.Join(errs...); err != nil {
		return nil, e.fail(err)
	}
	return &pb.CloseResponse{}, nil
}

// Ping implements pb.DriverServer.
func (s *Server) Ping(ctx context.Context, req *pb.PingRequest) (*pb.PingResponse, error) {
	e, err := s.conn(req.GetConn())
	if err != nil {
		return nil, err
	}
	if err := e.conn.Ping(ctx); err != nil {
		return nil, e.fail(err)
	}
	return &pb.PingResponse{}, nil
}

// Namespaces implements pb.DriverServer.
func (s *Server) Namespaces(ctx context.Context, req *pb.NamespacesRequest) (*pb.NamespacesResponse, error) {
	e, err := s.conn(req.GetConn())
	if err != nil {
		return nil, err
	}
	nss, err := e.conn.Namespaces(ctx)
	if err != nil {
		return nil, e.fail(err)
	}
	p, err := convert.NamespacesToProto(nss)
	if err != nil {
		return nil, e.fail(err)
	}
	return &pb.NamespacesResponse{Namespaces: p}, nil
}

// Collections implements pb.DriverServer.
func (s *Server) Collections(ctx context.Context, req *pb.CollectionsRequest) (*pb.CollectionsResponse, error) {
	e, err := s.conn(req.GetConn())
	if err != nil {
		return nil, err
	}
	cs, err := e.conn.Collections(ctx, driver.NamespaceRef{Name: req.GetNamespace()})
	if err != nil {
		return nil, e.fail(err)
	}
	p, err := convert.CollectionsToProto(cs)
	if err != nil {
		return nil, e.fail(err)
	}
	return &pb.CollectionsResponse{Collections: p}, nil
}

// Describe implements pb.DriverServer.
func (s *Server) Describe(ctx context.Context, req *pb.DescribeRequest) (*pb.DescribeResponse, error) {
	e, err := s.conn(req.GetConn())
	if err != nil {
		return nil, err
	}
	ref, err := convert.CollectionRefFromProto(req.GetRef())
	if err != nil {
		return nil, e.fail(err)
	}
	schema, err := e.conn.Describe(ctx, ref)
	if err != nil {
		return nil, e.fail(err)
	}
	p, err := convert.SchemaToProto(schema)
	if err != nil {
		return nil, e.fail(err)
	}
	return &pb.DescribeResponse{Schema: p}, nil
}
