// Command mcp-client starts `tobira mcp` over stdio, lists the tools it
// offers, calls describe and prints the result. It is the smallest useful MCP
// client for tobira, written with github.com/mark3labs/mcp-go.
//
// Usage:
//
//	go run . -connection local-pg -collection users
//	go run . -tobira ./tobira -config ./config.yaml -connection lite -collection t
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

type options struct {
	tobira     string
	config     string
	connection string
	collection string
	namespace  string
	env        []string
}

func main() {
	var o options
	flag.StringVar(&o.tobira, "tobira", "tobira", "path to the tobira binary")
	flag.StringVar(&o.config, "config", "", "tobira config file (default: tobira's own lookup)")
	flag.StringVar(&o.connection, "connection", "", "connection to expose and describe (required)")
	flag.StringVar(&o.collection, "collection", "", "collection to describe (required)")
	flag.StringVar(&o.namespace, "namespace", "", "namespace of the collection (optional)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Stdout, o); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		stop()
		os.Exit(1)
	}
}

// run starts the server, lists its tools and prints describe's result to w.
func run(ctx context.Context, w io.Writer, o options) error {
	if o.connection == "" || o.collection == "" {
		return errors.New("-connection and -collection are required")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// --expose is the whole access policy: the server sees only this connection.
	var args []string
	if o.config != "" {
		args = append(args, "--config", o.config)
	}
	args = append(args, "mcp", "--expose", o.connection)

	c, err := client.NewStdioMCPClient(o.tobira, o.env, args...)
	if err != nil {
		return fmt.Errorf("start %s: %w", o.tobira, err)
	}
	defer func() { _ = c.Close() }()

	var init mcp.InitializeRequest
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "tobira-example-client", Version: "0.1.0"}
	if _, err := c.Initialize(ctx, init); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}

	tools, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return fmt.Errorf("list tools: %w", err)
	}
	var out strings.Builder
	out.WriteString("tools:\n")
	for _, t := range tools.Tools {
		out.WriteString("  " + t.Name + "\n")
	}

	args2 := map[string]any{"connection": o.connection, "collection": o.collection}
	if o.namespace != "" {
		args2["namespace"] = o.namespace
	}
	var req mcp.CallToolRequest
	req.Params.Name = "describe"
	req.Params.Arguments = args2
	res, err := c.CallTool(ctx, req)
	if err != nil {
		return fmt.Errorf("describe: %w", err)
	}
	var text strings.Builder
	for _, content := range res.Content {
		if tc, ok := content.(mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	if res.IsError {
		return fmt.Errorf("describe failed: %s", text.String())
	}
	out.WriteString("describe:\n" + text.String() + "\n")
	_, err = io.WriteString(w, out.String())
	return err
}
