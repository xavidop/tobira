// Package shared holds what both ends of a tobira driver plugin must agree
// on: the go-plugin handshake (protocol version and magic cookie) and the
// name under which the Driver service is dispensed. The SDK
// (plugin/sdk.Serve) serves with it and the host (plugin/host) launches
// plugins with it, so the two can never drift apart.
package shared

import (
	"context"
	"errors"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
)

const (
	// ProtocolVersion is the plugin protocol version: tobira.plugin.v1.
	// go-plugin refuses a plugin built for another version with a clear
	// "incompatible API version" error.
	ProtocolVersion = 1
	// MagicCookieKey is the environment variable the host sets to
	// MagicCookieValue when it launches a plugin. It falls under the
	// TOBIRA_PLUGIN_ prefix of the plugin env allow-list.
	MagicCookieKey = "TOBIRA_PLUGIN_MAGIC"
	// MagicCookieValue is the fixed cookie value. It is not a secret: it
	// only stops a plugin binary that is run by hand from waiting for a
	// host that will never come.
	MagicCookieValue = "3da3a621a07bc87a23ec76a6272a831b3d07167954b657b93dc176678328f837"
	// PluginName is the key of the Driver service in the plugin set.
	PluginName = "driver"
)

// Handshake is the go-plugin handshake both ends use.
func Handshake() plugin.HandshakeConfig {
	return plugin.HandshakeConfig{
		ProtocolVersion:  ProtocolVersion,
		MagicCookieKey:   MagicCookieKey,
		MagicCookieValue: MagicCookieValue,
	}
}

// DriverPlugin is the go-plugin GRPCPlugin for the Driver service. The SDK
// sets Impl, the driver server it registers; the host leaves it nil and
// dispenses a pb.DriverClient. net/rpc is not supported.
type DriverPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	// Impl is the plugin side's implementation of the service.
	Impl pb.DriverServer
}

// GRPCServer registers p.Impl on s.
func (p *DriverPlugin) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	if p.Impl == nil {
		return errors.New("shared: DriverPlugin has no Impl")
	}
	pb.RegisterDriverServer(s, p.Impl)
	return nil
}

// GRPCClient returns a pb.DriverClient on c.
func (p *DriverPlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return pb.NewDriverClient(c), nil
}

// PluginSet is the plugin set with the Driver service under PluginName. The
// SDK passes its server; the host passes nil.
func PluginSet(server pb.DriverServer) plugin.PluginSet {
	return plugin.PluginSet{PluginName: &DriverPlugin{Impl: server}}
}
