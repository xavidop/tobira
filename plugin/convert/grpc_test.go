package convert_test

import (
	"context"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/xavidop/tobira/plugin/convert"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
)

// bigServer answers Info with a large version string and accepts any Open.
type bigServer struct {
	pb.UnimplementedDriverServer
	size int
}

func (s bigServer) Info(context.Context, *pb.InfoRequest) (*pb.InfoResponse, error) {
	return &pb.InfoResponse{Info: &pb.DriverInfo{Version: strings.Repeat("v", s.size)}}, nil
}

func (bigServer) Open(context.Context, *pb.OpenRequest) (*pb.OpenResponse, error) {
	return &pb.OpenResponse{Conn: &pb.ConnHandle{Id: "c"}}, nil
}

func dial(t *testing.T, server []grpc.ServerOption, client []grpc.DialOption, size int) pb.DriverClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(server...)
	pb.RegisterDriverServer(srv, bigServer{size: size})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	opts := append([]grpc.DialOption{
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, client...)
	cc, err := grpc.NewClient("passthrough:///bufnet", opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return pb.NewDriverClient(cc)
}

func TestGRPCOptionsRaiseTheMessageCap(t *testing.T) {
	// 10 MiB: over gRPC's 4 MiB default, under MaxMessageBytes.
	const size = 10 << 20
	ctx := context.Background()
	big := &pb.OpenRequest{Config: &pb.ConnConfig{Dsn: strings.Repeat("d", size)}}

	c := dial(t, convert.GRPCServerOptions(), convert.GRPCDialOptions(), size)
	if _, err := c.Info(ctx, &pb.InfoRequest{}); err != nil {
		t.Fatalf("Info with a 10 MiB response: %v", err)
	}
	if _, err := c.Open(ctx, big); err != nil {
		t.Fatalf("Open with a 10 MiB request: %v", err)
	}

	d := dial(t, nil, nil, size)
	if _, err := d.Info(ctx, &pb.InfoRequest{}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("default options, 10 MiB response: err = %v, want ResourceExhausted", err)
	}
}

func TestGRPCOptionsKeepTheCap(t *testing.T) {
	c := dial(t, convert.GRPCServerOptions(), convert.GRPCDialOptions(), convert.MaxMessageBytes+1)
	if _, err := c.Info(context.Background(), &pb.InfoRequest{}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("response over MaxMessageBytes: err = %v, want ResourceExhausted", err)
	}
}
