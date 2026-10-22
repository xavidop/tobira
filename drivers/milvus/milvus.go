package milvus

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"

	"github.com/xavidop/tobira/driver"
)

// Name is the registry name of the driver.
const Name = "milvus"

func init() { driver.Register(New()) }

type milvusDriver struct{}

// New returns the milvus driver.
func New() driver.Driver { return milvusDriver{} }

func (milvusDriver) Info() driver.Info {
	return driver.Info{
		Name:         Name,
		Kind:         driver.KindVector,
		Schemes:      []string{"milvus"},
		Dialect:      "milvus",
		Version:      "0.1.0-dev",
		Capabilities: []driver.Capability{driver.CapBrowser, driver.CapMutator, driver.CapVectorSearcher, driver.CapReadOnly},
	}
}

var errClosed = errors.New("connection is closed")

// conn is an open Milvus connection. It is safe for concurrent use: every
// request names its database, and conn keeps no per-call state. Requests
// go straight to the Milvus gRPC service (milvus-proto), with the SDK's
// column and entity packages for the wire formats. The SDK's milvusclient
// package is not used: it links Milvus's server packages, an embedded
// etcd server among them, whose otelgrpc API no longer builds next to the
// other drivers' dependencies; and it builds some expressions by string
// concatenation. The driver builds every filter itself, with template
// values.
type conn struct {
	svc       milvuspb.MilvusServiceClient
	closeFn   func() error
	defaultDB string
	readOnly  bool
	embedder  driver.Embedder
	closed    atomic.Bool
}

var (
	_ driver.Conn           = (*conn)(nil)
	_ driver.Browser        = (*conn)(nil)
	_ driver.Mutator        = (*conn)(nil)
	_ driver.VectorSearcher = (*conn)(nil)
)

func newConn(svc milvuspb.MilvusServiceClient, closeFn func() error, db string, cfg driver.ConnConfig) *conn {
	return &conn{svc: svc, closeFn: closeFn, defaultDB: db, readOnly: cfg.ReadOnly, embedder: cfg.Embedder}
}

// check fails a call on a closed connection or a done context.
func (c *conn) check(ctx context.Context, op string) error {
	if c.closed.Load() {
		return driver.Wrap(driver.ErrConnection, Name, op, errClosed)
	}
	if err := ctx.Err(); err != nil {
		return driver.Wrap(nil, Name, op, err)
	}
	return nil
}

// db resolves a namespace to a database name: "" is the connection's
// database.
func (c *conn) db(ns string) string {
	if ns == "" {
		return c.defaultDB
	}
	return ns
}

// Ping lists the collections of the connection's database: it needs a
// valid token, and fails when the database is gone.
func (c *conn) Ping(ctx context.Context) error {
	if err := c.check(ctx, "ping"); err != nil {
		return err
	}
	resp, err := c.svc.ShowCollections(ctx, &milvuspb.ShowCollectionsRequest{DbName: c.defaultDB})
	return mapError(ctx, "ping", checkRPC(resp, err))
}

// Close closes the gRPC connection. It is idempotent.
func (c *conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	if c.closeFn == nil {
		return nil
	}
	return mapError(context.Background(), "close", c.closeFn())
}
