package snowflake

import (
	"context"
	"io"

	sf "github.com/snowflakedb/gosnowflake/v2"
	"github.com/snowflakedb/gosnowflake/v2/sflog"
)

// gosnowflake logs through one process-wide logger, which writes to stderr
// by default (it would draw over the TUI) and which a Snowflake client
// config file (sf_client_config.json, found through SF_CLIENT_CONFIG_FILE
// or the home directory: "easy logging") can point at a log directory at
// DEBUG level. Its messages are not tied to a connection, so they cannot
// be routed to ConnConfig.Logger. silenceSDKLogging, run once from init,
// installs discardLogger instead: it drops every message, and since it is
// not gosnowflake's own logger type, easy logging refuses to replace it.

func silenceSDKLogging() { _ = sf.SetLogger(discardLogger{}) }

// discardLogger is an sf.SFLogger that drops everything.
type discardLogger struct{}

var _ sf.SFLogger = discardLogger{}

func (discardLogger) Tracef(string, ...any) {}
func (discardLogger) Debugf(string, ...any) {}
func (discardLogger) Infof(string, ...any)  {}
func (discardLogger) Warnf(string, ...any)  {}
func (discardLogger) Errorf(string, ...any) {}
func (discardLogger) Fatalf(string, ...any) {}
func (discardLogger) Trace(string)          {}
func (discardLogger) Debug(string)          {}
func (discardLogger) Info(string)           {}
func (discardLogger) Warn(string)           {}
func (discardLogger) Error(string)          {}
func (discardLogger) Fatal(string)          {}

func (d discardLogger) WithField(string, any) sf.LogEntry       { return d }
func (d discardLogger) WithFields(map[string]any) sf.LogEntry   { return d }
func (d discardLogger) WithContext(context.Context) sf.LogEntry { return d }
func (discardLogger) SetLogLevel(string) error                  { return nil }
func (discardLogger) SetLogLevelInt(sf.Level) error             { return nil }
func (discardLogger) GetLogLevel() string                       { return "OFF" }
func (discardLogger) GetLogLevelInt() sf.Level                  { return sflog.LevelOff }
func (discardLogger) SetOutput(io.Writer)                       {}
