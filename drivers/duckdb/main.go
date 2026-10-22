// Command tobira-driver-duckdb is tobira's DuckDB driver, shipped as an
// external plugin: DuckDB needs cgo, and tobira's release binaries are
// built with CGO_ENABLED=0. Install the release binary for your platform
// with `tobira driver install <url> --sha256 <hex>`; tobira then offers
// duckdb:// connections. See README.md.
package main

import (
	"github.com/xavidop/tobira/drivers/duckdb/internal/duckdb"
	"github.com/xavidop/tobira/plugin/sdk"
)

func main() { sdk.Serve(duckdb.New()) }
