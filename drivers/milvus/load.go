package milvus

import (
	"context"
	"fmt"
	"time"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"

	"github.com/xavidop/tobira/driver"
)

// loadPoll is the interval between load-state checks while a writable
// connection loads a collection.
const loadPoll = 200 * time.Millisecond

// withLoad runs call, a read that needs the collection loaded in memory.
// When Milvus says the collection is not loaded, a read-only request
// fails with a hint and never loads it (loading allocates memory on the
// server and changes its state). A writable one loads it, waits until it
// is loaded and runs call once more.
func (c *conn) withLoad(ctx context.Context, op string, s *collSchema, readOnly bool, call func() error) error {
	err := call()
	if !notLoaded(err) {
		return mapError(ctx, op, err)
	}
	if readOnly {
		return driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf(
			"collection %s is not loaded, and a read-only connection never loads a collection: load it from a writable connection (opening it there loads it) or with another Milvus client", s.name))
	}
	if err := c.load(ctx, op, s); err != nil {
		return err
	}
	return mapError(ctx, op, call())
}

// load loads collection s and waits until Milvus reports it loaded.
func (c *conn) load(ctx context.Context, op string, s *collSchema) error {
	st, err := c.svc.LoadCollection(ctx, &milvuspb.LoadCollectionRequest{DbName: s.db, CollectionName: s.name})
	if err := checkRPC(st, err); err != nil {
		mapped := mapError(ctx, op, err)
		if codeOf(err) == codeIndexNotFound {
			return driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("collection %s cannot be loaded: every vector field needs an index first (%v)", s.name, mapped))
		}
		return mapped
	}
	for {
		resp, err := c.svc.GetLoadState(ctx, &milvuspb.GetLoadStateRequest{DbName: s.db, CollectionName: s.name})
		if err := checkRPC(resp, err); err != nil {
			return mapError(ctx, op, err)
		}
		switch resp.GetState() {
		case commonpb.LoadState_LoadStateLoaded:
			return nil
		case commonpb.LoadState_LoadStateNotExist:
			return driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("collection %s does not exist", s.name))
		}
		t := time.NewTimer(loadPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			return driver.Wrap(nil, Name, op, ctx.Err())
		case <-t.C:
		}
	}
}
