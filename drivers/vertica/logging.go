package vertica

import vlogger "github.com/vertica/vertica-sql-go/logger"

// vertica-sql-go logs through one process-wide logger that writes to
// stdout at WARN by default (it would draw over the TUI), can be pointed
// at a file through VERTICA_SQL_GO_LOG_FILE and VERTICA_SQL_GO_LOG_LEVEL
// when the package loads, and calls os.Exit after a FATAL message. Its
// messages are not tied to a connection, so they cannot be routed to
// ConnConfig.Logger. silenceSDKLogging, run once from init (after the
// SDK's own init), sets the level to NONE, so nothing is formatted, written
// or exited on, and replaces the backend with one that drops everything,
// which also closes a log file the environment opened.
func silenceSDKLogging() {
	vlogger.SetLogLevel(vlogger.NONE)
	vlogger.SetLogger(discardLogger{})
}

// discardLogger is a vertica-sql-go logger backend that drops everything.
type discardLogger struct{}

func (discardLogger) Write(string, string, string) {}
func (discardLogger) Close()                       {}
