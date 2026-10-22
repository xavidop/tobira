package shared_test

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
	"github.com/xavidop/tobira/plugin/shared"
)

func TestHandshake(t *testing.T) {
	h := shared.Handshake()
	if h.ProtocolVersion != 1 || shared.ProtocolVersion != 1 {
		t.Fatalf("protocol version %d", h.ProtocolVersion)
	}
	if h.MagicCookieKey != "TOBIRA_PLUGIN_MAGIC" || h.MagicCookieValue != shared.MagicCookieValue || len(h.MagicCookieValue) < 32 {
		t.Fatalf("cookie %q=%q", h.MagicCookieKey, h.MagicCookieValue)
	}
	// The env allow-list passes TOBIRA_PLUGIN_*, so the cookie reaches the plugin.
	if !strings.HasPrefix(h.MagicCookieKey, "TOBIRA_PLUGIN_") {
		t.Fatal("cookie key must fall under the TOBIRA_PLUGIN_ allow-list prefix")
	}
	if shared.Handshake() != h {
		t.Fatal("Handshake must be constant")
	}
}

type infoServer struct{ pb.UnimplementedDriverServer }

func (infoServer) Info(context.Context, *pb.InfoRequest) (*pb.InfoResponse, error) {
	return &pb.InfoResponse{Info: &pb.DriverInfo{Name: "probe"}}, nil
}

func TestDriverPluginServesAndDispenses(t *testing.T) {
	var p plugin.GRPCPlugin = &shared.DriverPlugin{Impl: infoServer{}}
	set := shared.PluginSet(infoServer{})
	if _, ok := set[shared.PluginName].(*shared.DriverPlugin); !ok {
		t.Fatalf("plugin set %v lacks %q", set, shared.PluginName)
	}

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	if err := p.GRPCServer(nil, srv); err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	raw, err := p.GRPCClient(context.Background(), nil, cc)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := raw.(pb.DriverClient)
	if !ok {
		t.Fatalf("GRPCClient returned %T, want pb.DriverClient", raw)
	}
	resp, err := client.Info(context.Background(), &pb.InfoRequest{})
	if err != nil || resp.GetInfo().GetName() != "probe" {
		t.Fatalf("Info: %v, %v", resp, err)
	}
}

func TestDriverPluginWithoutServer(t *testing.T) {
	p := &shared.DriverPlugin{}
	if err := p.GRPCServer(nil, grpc.NewServer()); err == nil {
		t.Fatal("serving a nil driver server must fail")
	}
}
