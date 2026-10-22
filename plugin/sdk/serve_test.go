package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/plugin/convert"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
	"github.com/xavidop/tobira/plugin/shared"
)

const (
	// childEnv marks the test binary re-executed as a real plugin process.
	childEnv = "TOBIRA_PLUGIN_SDK_TEST_CHILD"
	// markerEnv names a file the child plugin creates when a Conn closes.
	markerEnv = "TOBIRA_PLUGIN_SDK_TEST_MARKER"
	// parentEnv marks the test binary re-executed as a host that launches
	// the child plugin, opens a Conn, prints the plugin's pid and waits to
	// be killed. Its value is the marker path to hand the child.
	parentEnv = "TOBIRA_PLUGIN_SDK_TEST_PARENT"
)

func TestMain(m *testing.M) {
	switch {
	case os.Getenv(childEnv) == "1":
		d := drivertest.NewFake(drivertest.WithPeople()).Driver()
		if marker := os.Getenv(markerEnv); marker != "" {
			d = markerDriver{Driver: d, path: marker}
		}
		Serve(d)
		return
	case os.Getenv(parentEnv) != "":
		runParent(os.Getenv(parentEnv))
		return
	}
	os.Exit(m.Run())
}

// markerDriver creates path when one of its Conns is closed.
type markerDriver struct {
	driver.Driver
	path string
}

type markerConn struct {
	driver.Conn
	path string
}

func (c markerConn) Close() error {
	_ = os.WriteFile(c.path, []byte("closed"), 0o600)
	return c.Conn.Close()
}

func (d markerDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	c, err := d.Driver.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return markerConn{Conn: c, path: d.path}, nil
}

// runParent plays a host: it launches the child plugin, opens a Conn,
// prints the plugin's pid and blocks until it is killed.
func runParent(marker string) {
	exe, err := os.Executable()
	if err != nil {
		os.Exit(2)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = []string{childEnv + "=1", markerEnv + "=" + marker, "PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + os.TempDir()}
	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  shared.Handshake(),
		Plugins:          shared.PluginSet(nil),
		Cmd:              cmd,
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		AutoMTLS:         true,
		GRPCDialOptions:  convert.GRPCDialOptions(),
		Logger:           hclog.NewNullLogger(),
		Stderr:           io.Discard,
		SyncStdout:       io.Discard,
	})
	rpc, err := client.Client()
	if err != nil {
		os.Exit(3)
	}
	raw, err := rpc.Dispense(shared.PluginName)
	if err != nil {
		os.Exit(4)
	}
	if _, err := raw.(pb.DriverClient).Open(context.Background(), &pb.OpenRequest{Config: &pb.ConnConfig{}}); err != nil {
		os.Exit(5)
	}
	_, _ = os.Stdout.WriteString(strconv.Itoa(client.ReattachConfig().Pid) + "\n")
	select {}
}

func dispense(t *testing.T, client *plugin.Client) pb.DriverClient {
	t.Helper()
	rpc, err := client.Client()
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	raw, err := rpc.Dispense(shared.PluginName)
	if err != nil {
		t.Fatalf("dispense: %v", err)
	}
	dc, ok := raw.(pb.DriverClient)
	if !ok {
		t.Fatalf("dispensed %T, want pb.DriverClient", raw)
	}
	return dc
}

// TestServeSpeaksTheHandshake runs Serve in go-plugin's in-process test mode
// and reattaches a client with the shared handshake and plugin set.
func TestServeSpeaksTheHandshake(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reattach := make(chan *plugin.ReattachConfig, 1)
	closed := make(chan struct{})
	go serve(drivertest.NewFake().Driver(), io.Discard, &plugin.ServeTestConfig{
		Context: ctx, ReattachConfigCh: reattach, CloseCh: closed,
	})
	var rc *plugin.ReattachConfig
	select {
	case rc = <-reattach:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve never became ready")
	}
	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  shared.Handshake(),
		Plugins:          shared.PluginSet(nil),
		Reattach:         rc,
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		GRPCDialOptions:  convert.GRPCDialOptions(),
		Logger:           hclog.NewNullLogger(),
	})
	defer client.Kill()
	dc := dispense(t, client)
	info, err := dc.Info(context.Background(), &pb.InfoRequest{})
	if err != nil || info.GetInfo().GetName() != "fake" {
		t.Fatalf("Info: %v, %v", info, err)
	}
	cancel()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not stop when its context ended")
	}
}

// TestServeAsAPluginProcess launches this test binary as a real plugin:
// the cookie check, version negotiation, AutoMTLS and the stdout handshake
// all run as they do under the host.
func TestServeAsAPluginProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a subprocess")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = []string{childEnv + "=1", "PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + os.TempDir()}
	var stderr bytes.Buffer
	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  shared.Handshake(),
		Plugins:          shared.PluginSet(nil),
		Cmd:              cmd,
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		AutoMTLS:         true,
		GRPCDialOptions:  convert.GRPCDialOptions(),
		Logger:           hclog.NewNullLogger(),
		Stderr:           &stderr,
		SyncStdout:       io.Discard,
	})
	defer client.Kill()
	dc := dispense(t, client)
	ctx := context.Background()
	info, err := dc.Info(ctx, &pb.InfoRequest{})
	if err != nil || info.GetInfo().GetName() != "fake" {
		t.Fatalf("Info: %v, %v (stderr: %s)", info, err, stderr.String())
	}
	resp, err := dc.Open(ctx, &pb.OpenRequest{Config: &pb.ConnConfig{}})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := dc.Query(ctx, &pb.QueryRequest{Conn: resp.GetConn(), Query: &pb.Query{Text: "select * from tobira_people"}})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := convert.NewFrameResultSet(ctx, stream.Recv, func() {})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for {
		_, ok, err := rs.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		n++
	}
	_ = rs.Close()
	if n != len(drivertest.People.Rows) {
		t.Fatalf("%d rows, want %d", n, len(drivertest.People.Rows))
	}
}

func TestPluginRefusesToRunWithoutTheCookie(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a subprocess")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = []string{childEnv + "=1"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("a plugin run by hand must exit non-zero")
	}
	if stdout.Len() != 0 {
		t.Fatalf("wrote to stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "This binary is a plugin") {
		t.Fatalf("stderr: %q", stderr.String())
	}
}

func TestLoggerWritesHclogJSON(t *testing.T) {
	var buf bytes.Buffer
	newLogger(&buf, slog.LevelInfo).Warn("hello", "conn", "prod", "n", 3)
	line := bytes.TrimSpace(buf.Bytes())
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("not JSON: %q", line)
	}
	if m["@message"] != "hello" || m["@level"] != "warn" || m["conn"] != "prod" {
		t.Fatalf("fields %v", m)
	}
	ts, _ := m["@timestamp"].(string)
	if _, err := time.Parse("2006-01-02T15:04:05.000000Z07:00", ts); err != nil {
		t.Fatalf("go-plugin cannot parse @timestamp %q: %v", ts, err)
	}
	newLogger(&buf, slog.LevelInfo).Debug("quiet")
	if strings.Contains(buf.String(), "quiet") {
		t.Fatal("debug is below the info level")
	}
	newLogger(&buf, slog.LevelDebug).Debug("loud")
	if !strings.Contains(buf.String(), `"@level":"debug"`) {
		t.Fatalf("debug record missing: %s", buf.String())
	}
}

func TestLogLevelFromEnv(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"": slog.LevelInfo, "info": slog.LevelInfo, "debug": slog.LevelDebug, "DEBUG": slog.LevelDebug,
		"warn": slog.LevelWarn, "error": slog.LevelError, "nonsense": slog.LevelInfo,
	} {
		if got := parseLevel(in); got != want {
			t.Errorf("parseLevel(%q) = %v, want %v", in, got, want)
		}
	}
	if logLevelEnv != "TOBIRA_PLUGIN_LOG_LEVEL" {
		t.Fatalf("env var %q", logLevelEnv)
	}
}

func TestWatchParentFiresWhenThePPIDChanges(t *testing.T) {
	var ppid atomic.Int32
	ppid.Store(100)
	fired := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchParent(ctx, time.Millisecond, 100, func() int { return int(ppid.Load()) }, func() { close(fired) })
	select {
	case <-fired:
		t.Fatal("fired while the parent is alive")
	case <-time.After(20 * time.Millisecond):
	}
	ppid.Store(1)
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("never noticed the parent died")
	}

	// It stops with its context.
	ctx2, cancel2 := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		watchParent(ctx2, time.Millisecond, 7, func() int { return 7 }, func() { t.Error("fired") })
		close(stopped)
	}()
	cancel2()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("watchParent ignored its context")
	}
}

// The parent pid is the one serve recorded when it started, so a host that
// died before the watcher's first look is still noticed.
func TestWatchParentFiresWhenTheParentDiedBeforeItStarted(t *testing.T) {
	fired := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchParent(ctx, time.Millisecond, 100, func() int { return 1 }, func() { close(fired) })
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("never noticed the parent was already gone")
	}
}
