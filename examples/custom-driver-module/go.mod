module github.com/xavidop/tobira/examples/custom-driver-module

go 1.26.9

require github.com/xavidop/tobira v0.0.0-00010101000000-000000000000

require go.uber.org/goleak v1.3.0 // indirect

replace github.com/xavidop/tobira => ../..
