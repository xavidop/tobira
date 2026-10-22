package sdk

import (
	"context"
	"fmt"
	"runtime/debug"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/plugin/convert"
)

// panicMessage is all the host learns about a panic: the panic value can
// quote a DSN or row data.
const panicMessage = "plugin panic"

// ServerOptions are the gRPC server options a Server must be served with:
// the protocol's message caps (convert.GRPCServerOptions) and interceptors
// that turn a panic in a driver call into an Internal error, so one bad
// call does not take down the plugin and every other Conn and Tx it holds.
// Serve uses them; pass them to grpc.NewServer when serving a Server
// yourself. A panic in a goroutine the driver starts itself cannot be
// recovered and still crashes the plugin.
func (s *Server) ServerOptions() []grpc.ServerOption {
	return append(convert.GRPCServerOptions(),
		grpc.ChainUnaryInterceptor(s.recoverUnary),
		grpc.ChainStreamInterceptor(s.recoverStream),
	)
}

func (s *Server) recoverUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			resp, err = nil, s.panicked(info.FullMethod, r)
		}
	}()
	return handler(ctx, req)
}

func (s *Server) recoverStream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = s.panicked(info.FullMethod, r)
		}
	}()
	return handler(srv, ss)
}

// panicked logs a recovered panic with its stack to the server's logger
// (stderr), redacted with every live ConnConfig, and returns the generic
// status the host gets.
func (s *Server) panicked(method string, r any) error {
	detail, stack := fmt.Sprint(r), string(debug.Stack())
	for _, cfg := range s.liveConfigs() {
		detail, stack = driver.RedactMessage(detail, cfg), driver.RedactMessage(stack, cfg)
	}
	s.log.Error("driver panic", "method", method, "panic", detail, "stack", stack)
	return convert.StatusError(codes.Internal, nil, panicMessage)
}

// liveConfigs snapshots the ConnConfig of every open Conn.
func (s *Server) liveConfigs() []driver.ConnConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]driver.ConnConfig, 0, len(s.conns))
	for _, e := range s.conns {
		out = append(out, e.cfg)
	}
	return out
}
