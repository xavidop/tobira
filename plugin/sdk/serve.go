package sdk

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/plugin/shared"
)

// Serve serves d as a tobira driver plugin and returns when the host shuts
// the plugin down; call it from main and nothing else. It performs the
// go-plugin handshake (protocol version 1, the cookie in plugin/shared,
// gRPC only, AutoMTLS when the host asks for it), serves the Driver
// service and, on shutdown, rolls back every open Tx and closes every
// Conn.
//
// If the host dies without shutting the plugin down (it crashed or was
// killed), the plugin notices within a second that it was orphaned, rolls
// back and closes everything the same way and exits with status 0, so it
// never holds database connections or locks for a host that is gone.
//
// Logs are JSON lines on stderr at the level TOBIRA_PLUGIN_LOG_LEVEL names
// (debug, info, warn or error; info when unset).
//
// Run by hand (without the host's magic cookie) it prints an explanation
// to stderr and exits with status 1; go-plugin, which owns stdout for the
// handshake, also exits the process on a startup failure.
func Serve(d driver.Driver) {
	serve(d, os.Stderr, nil)
}

// logLevelEnv names the variable that sets the plugin's log level. It is
// under the TOBIRA_PLUGIN_ prefix the host passes through.
const logLevelEnv = "TOBIRA_PLUGIN_LOG_LEVEL"

// parentPollInterval is how often Serve checks that its host is alive.
const parentPollInterval = time.Second

// parseLevel reads a TOBIRA_PLUGIN_LOG_LEVEL value; anything unknown is
// info.
func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// serve is Serve with the log destination and go-plugin's test mode
// exposed for tests. Logs go to the stderr file as it was before go-plugin
// redirects os.Stderr, so they reach the host's stderr capture directly.
// Outside test mode it also watches for the host's death.
func serve(d driver.Driver, stderr io.Writer, test *plugin.ServeTestConfig) {
	// Record the host first: if it dies while the plugin starts up, the
	// parent pid is already the reaper's by the time the watcher runs.
	parent := os.Getppid()
	level := parseLevel(os.Getenv(logLevelEnv))
	srv := NewServer(d, WithLogger(newLogger(stderr, level)))
	defer func() { _ = srv.Shutdown() }()
	if test == nil {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go watchParent(ctx, parentPollInterval, parent, os.Getppid, func() {
			// The host's end of our stdio pipes is gone: a write to them
			// must not kill us with SIGPIPE before the cleanup is done.
			signal.Ignore(syscall.SIGPIPE)
			_ = srv.Shutdown()
			os.Exit(0)
		})
	}
	hclogLevel := hclog.LevelFromString(level.String())
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: shared.Handshake(),
		Plugins:         shared.PluginSet(srv),
		GRPCServer: func(opts []grpc.ServerOption) *grpc.Server {
			return grpc.NewServer(append(opts, srv.ServerOptions()...)...)
		},
		Logger: hclog.New(&hclog.LoggerOptions{
			Name: "tobira-plugin", Level: hclogLevel, Output: stderr, JSONFormat: true,
		}),
		Test: test,
	})
}

// watchParent calls onDeath once getppid stops returning parent, which
// happens when the parent exits and the process is re-parented. It polls
// every interval until ctx ends.
func watchParent(ctx context.Context, interval time.Duration, parent int, getppid func() int, onDeath func()) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if getppid() != parent {
				onDeath()
				return
			}
		}
	}
}

// hclogTime is the @timestamp layout go-plugin's host parses.
const hclogTime = "2006-01-02T15:04:05.000000Z07:00"

// newLogger logs JSON lines to w in hclog's shape (@timestamp, @level,
// @message, then the attributes), so the go-plugin host files each line
// under its level in the tobira debug log. Records below level are dropped.
func newLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) > 0 {
				return a
			}
			switch a.Key {
			case slog.TimeKey:
				return slog.String("@timestamp", a.Value.Time().Format(hclogTime))
			case slog.LevelKey:
				return slog.String("@level", strings.ToLower(a.Value.String()))
			case slog.MessageKey:
				return slog.String("@message", a.Value.String())
			}
			return a
		},
	}))
}
