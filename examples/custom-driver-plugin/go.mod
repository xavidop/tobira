module github.com/xavidop/tobira/examples/custom-driver-plugin

go 1.26.10

require (
	github.com/xavidop/tobira v0.0.0-00010101000000-000000000000
	github.com/xavidop/tobira/plugin/sdk v0.0.0-00010101000000-000000000000
	go.uber.org/goleak v1.3.0
)

require (
	github.com/fatih/color v1.19.0 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/hashicorp/go-hclog v1.6.3 // indirect
	github.com/hashicorp/go-plugin v1.8.0 // indirect
	github.com/hashicorp/yamux v0.1.2 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/oklog/run v1.1.0 // indirect
	golang.org/x/net v0.61.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261019180522-ce908d29299b // indirect
	google.golang.org/grpc v1.84.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace (
	github.com/xavidop/tobira => ../..
	github.com/xavidop/tobira/plugin/sdk => ../../plugin/sdk
)
