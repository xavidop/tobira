package sdk

import (
	"bufio"
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/plugin/convert"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
)

var (
	errNoResult  = errors.New("driver returned no result set")
	errNoTx      = errors.New("driver returned no transaction")
	errNoChannel = errors.New("driver returned no watch channel")
	errNoOp      = fmt.Errorf("%w: TxMutate carries no operation", convert.ErrInvalid)
)

// exportChunkBytes is the most bytes one ExportChunk carries.
const exportChunkBytes = 256 << 10

// Similar implements pb.DriverServer.
func (s *Server) Similar(req *pb.SimilarRequest, stream grpc.ServerStreamingServer[pb.ResultFrame]) error {
	e, v, err := connCapability[driver.VectorSearcher](s, req.GetConn(), driver.CapVectorSearcher)
	if err != nil {
		return err
	}
	ref, err := convert.CollectionRefFromProto(req.GetRef())
	if err != nil {
		return e.fail(err)
	}
	q, err := convert.VectorQueryFromProto(req.GetQuery())
	if err != nil {
		return e.fail(err)
	}
	rs, err := v.Similar(stream.Context(), ref, q)
	return sendResult(stream, e.fail, rs, err)
}

// Range implements pb.DriverServer.
func (s *Server) Range(req *pb.RangeRequest, stream grpc.ServerStreamingServer[pb.ResultFrame]) error {
	e, r, err := connCapability[driver.TimeSeriesReader](s, req.GetConn(), driver.CapTimeSeriesReader)
	if err != nil {
		return err
	}
	ref, err := convert.CollectionRefFromProto(req.GetRef())
	if err != nil {
		return e.fail(err)
	}
	q, err := convert.RangeQueryFromProto(req.GetQuery())
	if err != nil {
		return e.fail(err)
	}
	rs, err := r.Range(stream.Context(), ref, q)
	return sendResult(stream, e.fail, rs, err)
}

// Neighbors implements pb.DriverServer.
func (s *Server) Neighbors(ctx context.Context, req *pb.NeighborsRequest) (*pb.NeighborsResponse, error) {
	e, g, err := connCapability[driver.GraphReader](s, req.GetConn(), driver.CapGraphReader)
	if err != nil {
		return nil, err
	}
	q, err := convert.GraphQueryFromProto(req.GetQuery())
	if err != nil {
		return nil, e.fail(err)
	}
	graph, err := g.Neighbors(ctx, q)
	if err != nil {
		return nil, e.fail(err)
	}
	p, err := convert.GraphToProto(graph)
	if err != nil {
		return nil, e.fail(err)
	}
	return &pb.NeighborsResponse{Graph: p}, nil
}

// chunkWriter sends what is written to it as ExportChunks of at most
// exportChunkBytes.
type chunkWriter struct {
	send func(*pb.ExportChunk) error
}

func (w chunkWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		k := min(len(p), exportChunkBytes)
		if err := w.send(&pb.ExportChunk{Data: p[:k]}); err != nil {
			return n, err
		}
		n += k
		p = p[k:]
	}
	return n, nil
}

// Export implements pb.DriverServer. The driver's bytes are buffered into
// chunks of up to 256 KiB and streamed in order.
func (s *Server) Export(req *pb.ExportRequest, stream grpc.ServerStreamingServer[pb.ExportChunk]) error {
	e, x, err := connCapability[driver.Exporter](s, req.GetConn(), driver.CapExporter)
	if err != nil {
		return err
	}
	q, err := convert.QueryFromProto(req.GetQuery())
	if err != nil {
		return e.fail(err)
	}
	w := bufio.NewWriterSize(chunkWriter{send: stream.Send}, exportChunkBytes)
	if err := x.Export(stream.Context(), q, req.GetFormat(), w); err != nil {
		return e.fail(err)
	}
	return e.fail(w.Flush())
}

// Watch implements pb.DriverServer. Response headers are sent once the
// driver's Watch is established, so a host can wait on them before making
// the changes it expects to see. A terminal event's error is redacted like
// any other. The stream ends when the driver closes its channel, which it
// does when the host cancels.
func (s *Server) Watch(req *pb.WatchRequest, stream grpc.ServerStreamingServer[pb.Event]) error {
	e, w, err := connCapability[driver.Watcher](s, req.GetConn(), driver.CapWatcher)
	if err != nil {
		return err
	}
	wr, err := convert.WatchRequestFromProto(req)
	if err != nil {
		return e.fail(err)
	}
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	ch, err := w.Watch(ctx, wr)
	if err != nil {
		return e.fail(err)
	}
	if ch == nil {
		return e.fail(errNoChannel)
	}
	// Whatever ends this call, keep draining until the driver closes the
	// channel (it does once ctx is cancelled), so it never blocks on a send.
	defer func() {
		cancel()
		go func() {
			for range ch {
			}
		}()
	}()
	if err := stream.SendHeader(metadata.MD{}); err != nil {
		return err
	}
	redact := redactor(e.cfg)
	for ev := range ch {
		pe, err := convert.EventToProto(ev, redact)
		if err != nil {
			return e.fail(err)
		}
		if err := stream.Send(pe); err != nil {
			return err
		}
	}
	return e.fail(stream.Context().Err())
}
