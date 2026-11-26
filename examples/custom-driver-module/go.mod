module github.com/xavidop/tobira/examples/custom-driver-module

go 1.26.11

require github.com/xavidop/tobira v0.0.0-00010101000000-000000000000

require (
	github.com/stretchr/testify v1.12.1 // indirect
	go.uber.org/goleak v1.3.0 // indirect
	go.yaml.in/yaml/v3 v3.0.6 // indirect
)

replace github.com/xavidop/tobira => ../..
