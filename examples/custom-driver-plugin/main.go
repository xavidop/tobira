// Command tobira-driver-memdemo is an example tobira driver plugin: the
// in-memory memdemo driver served over tobira's plugin protocol with the
// Go SDK. Build it as tobira-driver-memdemo and copy it into tobira's
// plugins directory; tobira then offers the memdemo driver. See README.md.
package main

import (
	"github.com/xavidop/tobira/examples/custom-driver-plugin/memdemo"
	"github.com/xavidop/tobira/plugin/sdk"
)

func main() { sdk.Serve(memdemo.New()) }
