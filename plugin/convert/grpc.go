package convert

import "google.golang.org/grpc"

// GRPCDialOptions are the client options both sides of the plugin boundary
// must use: send and receive caps of MaxMessageBytes instead of gRPC's 4 MiB
// receive default.
func GRPCDialOptions() []grpc.DialOption {
	return []grpc.DialOption{grpc.WithDefaultCallOptions(
		grpc.MaxCallRecvMsgSize(MaxMessageBytes),
		grpc.MaxCallSendMsgSize(MaxMessageBytes),
	)}
}

// GRPCServerOptions are the matching server options: send and receive caps
// of MaxMessageBytes.
func GRPCServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.MaxRecvMsgSize(MaxMessageBytes),
		grpc.MaxSendMsgSize(MaxMessageBytes),
	}
}
