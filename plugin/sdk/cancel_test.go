package sdk_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
)

// endlessRS yields rows forever until its context ends, then records that
// the driver saw the cancellation.
type endlessRS struct {
	n       int64
	stopped chan error // receives the ctx error the driver saw
	closed  atomic.Bool
}

func (r *endlessRS) Columns() []driver.Column {
	return []driver.Column{{Name: "n", Type: driver.TypeInt}}
}

func (r *endlessRS) Next(ctx context.Context) (driver.Row, bool, error) {
	if err := ctx.Err(); err != nil {
		select {
		case r.stopped <- err:
		default:
		}
		return nil, false, err
	}
	r.n++
	if r.n > 300 {
		// Past the first batch, slow down so the host's cancel lands mid-stream.
		select {
		case <-ctx.Done():
			return r.Next(ctx)
		case <-time.After(time.Millisecond):
		}
	}
	return driver.Row{driver.Int(r.n)}, true, nil
}

func (r *endlessRS) Stats() driver.Stats { return driver.Stats{} }

func (r *endlessRS) Close() error {
	r.closed.Store(true)
	return nil
}

func TestCancelMidStreamStopsTheDriver(t *testing.T) {
	f := newFake()
	rs := &endlessRS{stopped: make(chan error, 1)}
	f.OnQuery("endless", func(driver.Query) (driver.ResultSet, error) { return rs, nil })
	c, _ := start(t, f.Driver())
	h := open(t, c, &pb.ConnConfig{}).GetConn()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := c.Query(ctx, &pb.QueryRequest{Conn: h, Query: &pb.Query{Text: "endless"}})
	if err != nil {
		t.Fatal(err)
	}
	if fr, err := stream.Recv(); err != nil || fr.GetColumns() == nil {
		t.Fatalf("first frame %v, %v", fr, err)
	}
	if fr, err := stream.Recv(); err != nil || len(fr.GetRows().GetRows()) != 256 {
		t.Fatalf("second frame %v, %v", fr, err)
	}
	cancel()

	select {
	case err := <-rs.stopped:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("driver saw %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the driver never saw the host's cancel")
	}
	eventually(t, "the result set to be closed", rs.closed.Load)
}

func TestCancelStopsABlockedDriverCall(t *testing.T) {
	f := newFake()
	c, _ := start(t, f.Driver())
	h := open(t, c, &pb.ConnConfig{}).GetConn()

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := c.Query(ctx, &pb.QueryRequest{Conn: h, Query: &pb.Query{Text: "sleep 1m"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the query to start", func() bool { return hasCall(f, "query") })
	cancel()
	if _, err := stream.Recv(); err == nil {
		t.Fatal("a cancelled stream must fail")
	}
	eventually(t, "the driver call to end with the cancel", func() bool {
		for _, call := range f.Calls() {
			if call.Op == "query" && errors.Is(call.Err, context.Canceled) {
				return true
			}
		}
		return false
	})
}

// watchCtxDriver's Conns are Watchers that hand over the ctx their Watch
// was called with.
type watchCtxDriver struct {
	driver.Driver
	ctxs chan context.Context
}

type watchCtxConn struct {
	driver.Conn
	d *watchCtxDriver
}

func (c watchCtxConn) Watch(ctx context.Context, req driver.WatchRequest) (<-chan driver.Event, error) {
	c.d.ctxs <- ctx
	return c.Conn.(driver.Watcher).Watch(ctx, req)
}

func (d *watchCtxDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	c, err := d.Driver.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return watchCtxConn{Conn: c, d: d}, nil
}

func TestCancelEndsAWatch(t *testing.T) {
	d := &watchCtxDriver{Driver: newFake().Driver(), ctxs: make(chan context.Context, 1)}
	c, _ := start(t, d)
	h := open(t, c, &pb.ConnConfig{}).GetConn()

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := c.Watch(ctx, &pb.WatchRequest{Conn: h, Collection: ref(people)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Header(); err != nil {
		t.Fatal(err)
	}
	watchCtx := <-d.ctxs
	cancel()
	if err := drain(stream.Recv); err == nil {
		t.Fatal("a cancelled watch must fail")
	}
	select {
	case <-watchCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the driver's Watch context never ended")
	}
}
