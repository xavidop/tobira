package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"google.golang.org/grpc"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/plugin/convert"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
)

// Similar implements driver.VectorSearcher. A text query is embedded here,
// with ConnConfig.Embedder, because a plugin cannot call the host's
// embedder: the vector travels with the text. Without an embedder the
// text goes alone and the plugin answers as its driver does (usually
// ErrUnsupported with driver.NeedEmbedderHint).
func (c *conn) Similar(ctx context.Context, ref driver.CollectionRef, q driver.VectorQuery) (driver.ResultSet, error) {
	if err := c.need("similar", driver.CapVectorSearcher); err != nil {
		return nil, err
	}
	if len(q.Vector) == 0 && q.Text != "" && c.cfg.Embedder != nil {
		vecs, err := c.cfg.Embedder.Embed(ctx, []string{q.Text})
		if err != nil {
			return nil, redactErr(err, c.cfg)
		}
		if len(vecs) != 1 || len(vecs[0]) == 0 {
			return nil, driver.Wrap(nil, c.a.plugin.Name, "similar",
				fmt.Errorf("the embedder returned %d vectors for one text", len(vecs)))
		}
		q.Vector = vecs[0]
	}
	pq, err := convert.VectorQueryToProto(q)
	if err != nil {
		return nil, c.decodeErr("similar", err)
	}
	pref := convert.CollectionRefToProto(ref)
	return c.frames(ctx, "similar", func(sctx context.Context) (grpc.ServerStreamingClient[pb.ResultFrame], error) {
		return c.p.rpc.Similar(sctx, &pb.SimilarRequest{Conn: c.h, Ref: pref, Query: pq})
	})
}

// Range implements driver.TimeSeriesReader.
func (c *conn) Range(ctx context.Context, ref driver.CollectionRef, q driver.RangeQuery) (driver.ResultSet, error) {
	if err := c.need("range", driver.CapTimeSeriesReader); err != nil {
		return nil, err
	}
	pq, err := convert.RangeQueryToProto(q)
	if err != nil {
		return nil, c.decodeErr("range", err)
	}
	pref := convert.CollectionRefToProto(ref)
	return c.frames(ctx, "range", func(sctx context.Context) (grpc.ServerStreamingClient[pb.ResultFrame], error) {
		return c.p.rpc.Range(sctx, &pb.RangeRequest{Conn: c.h, Ref: pref, Query: pq})
	})
}

// Neighbors implements driver.GraphReader.
func (c *conn) Neighbors(ctx context.Context, q driver.GraphQuery) (*driver.Graph, error) {
	if err := c.need("neighbors", driver.CapGraphReader); err != nil {
		return nil, err
	}
	pq, err := convert.GraphQueryToProto(q)
	if err != nil {
		return nil, c.decodeErr("neighbors", err)
	}
	resp, err := c.p.rpc.Neighbors(ctx, &pb.NeighborsRequest{Conn: c.h, Query: pq})
	if err != nil {
		return nil, c.fail(ctx, "neighbors", err)
	}
	g, err := convert.GraphFromProto(resp.GetGraph())
	if err != nil {
		return nil, c.decodeErr("neighbors", err)
	}
	return g, nil
}

// ExportFormats implements driver.Exporter: the formats the plugin listed
// when the Conn opened.
func (c *conn) ExportFormats() []string { return slices.Clone(c.formats) }

// Export implements driver.Exporter, copying the plugin's chunks to w in
// order. Cancelling ctx cancels the stream.
func (c *conn) Export(ctx context.Context, q driver.Query, format string, w io.Writer) error {
	if err := c.need("export", driver.CapExporter); err != nil {
		return err
	}
	pq, err := convert.QueryToProto(q)
	if err != nil {
		return c.decodeErr("export", err)
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.p.rpc.Export(sctx, &pb.ExportRequest{Conn: c.h, Query: pq, Format: format})
	if err != nil {
		return c.fail(sctx, "export", err)
	}
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return c.fail(sctx, "export", err)
		}
		if _, err := w.Write(chunk.GetData()); err != nil {
			return err
		}
	}
}

// Watch implements driver.Watcher. It returns once the plugin's Watch is
// established (the plugin sends response headers then), so a failure to
// start watching is Watch's error. Events then arrive on the channel,
// which closes when ctx ends; a stream that breaks first ends with one
// event carrying the error.
func (c *conn) Watch(ctx context.Context, req driver.WatchRequest) (<-chan driver.Event, error) {
	if err := c.need("watch", driver.CapWatcher); err != nil {
		return nil, err
	}
	pr := convert.WatchRequestToProto(c.h, req)
	sctx, cancel := context.WithCancel(ctx)
	stream, err := c.p.rpc.Watch(sctx, pr)
	if err != nil {
		cancel()
		return nil, c.fail(sctx, "watch", err)
	}
	md, err := stream.Header()
	if err == nil && md == nil {
		// Ended without headers: the status says why.
		_, err = stream.Recv()
		if err == nil || errors.Is(err, io.EOF) {
			err = driver.Wrap(nil, c.a.plugin.Name, "watch", errors.New("the plugin ended the watch at once"))
		}
	}
	if err != nil {
		cancel()
		return nil, c.fail(sctx, "watch", err)
	}
	ch := make(chan driver.Event)
	go func() {
		defer close(ch)
		defer cancel()
		for {
			pe, err := stream.Recv()
			if err != nil {
				if sctx.Err() != nil || errors.Is(err, io.EOF) {
					return
				}
				c.sendEvent(sctx, ch, driver.Event{Err: c.fail(sctx, "watch", err)})
				return
			}
			ev, err := convert.EventFromProto(pe)
			if err != nil {
				c.sendEvent(sctx, ch, driver.Event{Err: c.decodeErr("watch", err)})
				return
			}
			if ev.Err != nil {
				ev.Err = redactErr(ev.Err, c.cfg)
			}
			if !c.sendEvent(sctx, ch, ev) {
				return
			}
		}
	}()
	return ch, nil
}

// sendEvent delivers ev unless ctx ends first.
func (c *conn) sendEvent(ctx context.Context, ch chan<- driver.Event, ev driver.Event) bool {
	select {
	case ch <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}
