package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// pluginEnv builds plugin/host's test plugin (the drivertest Fake with the
// People and vector fixtures, every capability, CapReadOnly and
// CapVectorSearcher included, and Kind "vector" through
// TOBIRA_PLUGIN_TEST_KIND) into a plugins dir as tobira-driver-real, adds
// tobira-driver-gone, which never answers, and writes a config with a
// connection on each plus extra. It returns the environment.
func pluginEnv(t *testing.T, env []string, extra string) []string {
	t.Helper()
	dir := t.TempDir()
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "tobira-driver-real"+exe), ".")
	cmd.Dir = filepath.Join("..", "..", "plugin", "host", "testdata", "plugin")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the test plugin: %v\n%s", err, out)
	}
	conns := "connections:\n  - name: plug\n    dsn: real://x\n"
	if runtime.GOOS != "windows" {
		if err := os.WriteFile(filepath.Join(dir, "tobira-driver-gone"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		conns += "  - name: gone\n    dsn: gone://x\n"
	}
	env = writeConfig(t, env, "plugins:\n  dirs: ["+dir+"]\n"+conns+extra)
	return withEnv(env, "TOBIRA_PLUGIN_TEST_KIND=vector")
}

// tobira mcp over a plugin connection: the plugin's real Info is loaded
// before the tools are built, so similar is registered, list_connections
// shows its kind and dialect, and query and sample work on their first
// call. A plugin that never answers is listed as unavailable.
func TestMCPEndToEndPlugin(t *testing.T) {
	bin := buildBinary(t)
	env := pluginEnv(t, isolatedEnv(t), "")
	expose := "plug"
	if runtime.GOOS != "windows" {
		expose += ",gone"
	}
	cmd := exec.Command(bin, "mcp", "--expose", expose)
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			t.Error("tobira mcp did not exit after stdin closed")
		}
		if t.Failed() {
			t.Logf("stderr:\n%s", stderr.String())
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := client.NewClient(transport.NewIO(stdout, stdin, io.NopCloser(strings.NewReader(""))))
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var init mcp.InitializeRequest
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "tobira-e2e", Version: "0.0.1"}
	if _, err := c.Initialize(ctx, init); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	lt, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range lt.Tools {
		names = append(names, tl.Name)
	}
	if !slices.Contains(names, "similar") {
		t.Errorf("tools %v lack similar for a vector-search plugin", names)
	}

	out, isErr := mcpCall(t, c, "query", map[string]any{"connection": "plug", "query": "select * from tobira_people where id = 7"})
	if isErr || !strings.Contains(out, "Margaret Hamilton") {
		t.Errorf("first query on the plugin = %s (error %v)", out, isErr)
	}
	out, isErr = mcpCall(t, c, "sample", map[string]any{"connection": "plug", "collection": "tobira_people", "limit": 2})
	if isErr || !strings.Contains(out, "Conan O'Brien") {
		t.Errorf("sample on the plugin = %s (error %v)", out, isErr)
	}

	out, isErr = mcpCall(t, c, "list_connections", nil)
	if isErr {
		t.Fatalf("list_connections: %s", out)
	}
	var conns []struct{ Name, Driver, Kind, Dialect, Status, Error string }
	if err := json.Unmarshal([]byte(out), &conns); err != nil {
		t.Fatalf("list_connections: %v\n%s", err, out)
	}
	byName := map[string]int{}
	for i, cn := range conns {
		byName[cn.Name] = i
	}
	if i, ok := byName["plug"]; !ok || conns[i].Kind != "vector" || conns[i].Dialect != "fake" || conns[i].Driver != "real" {
		t.Errorf("list_connections = %s, want plug with the plugin's kind and dialect", out)
	}
	if runtime.GOOS != "windows" {
		i, ok := byName["gone"]
		if !ok || conns[i].Status != "unavailable" || !strings.Contains(conns[i].Error, `driver plugin "gone"`) {
			t.Errorf("list_connections = %s, want gone unavailable with the reason", out)
		}
		out, isErr = mcpCall(t, c, "describe", map[string]any{"connection": "gone", "collection": "x"})
		if !isErr || !strings.Contains(out, `connection "gone" is unavailable`) {
			t.Errorf("describe on gone = %s (error %v), want unavailable", out, isErr)
		}
	}
}
