package qdrant

import (
	"context"
	"errors"
	"sync/atomic"

	pb "github.com/qdrant/go-client/qdrant"

	"github.com/xavidop/tobira/driver"
)

// Name is the registry name of the driver.
const Name = "qdrant"

// defaultNamespace is the only namespace: Qdrant has no grouping above
// collections.
const defaultNamespace = "default"

func init() { driver.Register(New()) }

type qdrantDriver struct{}

// New returns the qdrant driver.
func New() driver.Driver { return qdrantDriver{} }

func (qdrantDriver) Info() driver.Info {
	return driver.Info{
		Name:         Name,
		Kind:         driver.KindVector,
		Schemes:      []string{"qdrant"},
		Dialect:      "qdrant",
		Version:      "0.1.0-dev",
		Capabilities: []driver.Capability{driver.CapBrowser, driver.CapMutator, driver.CapVectorSearcher, driver.CapReadOnly},
	}
}

var errClosed = errors.New("connection is closed")

// conn is an open gRPC client. It is safe for concurrent use: the client
// multiplexes calls over its connection and conn keeps no per-call state.
type conn struct {
	client   *pb.Client
	readOnly bool
	embedder driver.Embedder
	// beforeInsertWrite, set by tests only, runs in Insert between the
	// check that the id is free and the write, to stage a lost race.
	beforeInsertWrite func()
	// afterInsertWrite, set by tests only, runs in Insert right after the
	// write, before the read-back.
	afterInsertWrite func()
	closed           atomic.Bool
}

var (
	_ driver.Conn           = (*conn)(nil)
	_ driver.Browser        = (*conn)(nil)
	_ driver.Mutator        = (*conn)(nil)
	_ driver.VectorSearcher = (*conn)(nil)
)

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

// Ping lists the collections: unlike the health check it needs a valid API
// key, so bad credentials fail here.
func (c *conn) Ping(ctx context.Context) error {
	if err := c.check(ctx, "ping"); err != nil {
		return err
	}
	_, err := c.client.GetCollectionsClient().List(ctx, &pb.ListCollectionsRequest{})
	return mapError(ctx, "ping", err)
}

// Close closes the gRPC connection. It is idempotent.
func (c *conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	return mapError(context.Background(), "close", c.client.Close())
}
