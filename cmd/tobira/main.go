// Command tobira is the tobira terminal database client.
package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"

	"github.com/xavidop/mamori"
	"github.com/xavidop/tobira/driver"
	// Each blank import registers the drivers named in its comment; postgres,
	// mysql and elasticsearch also register their flavors.
	_ "github.com/xavidop/tobira/drivers/bigquery"      // registers "bigquery"
	_ "github.com/xavidop/tobira/drivers/cassandra"     // registers "cassandra"
	_ "github.com/xavidop/tobira/drivers/chroma"        // registers "chroma"
	_ "github.com/xavidop/tobira/drivers/clickhouse"    // registers "clickhouse"
	_ "github.com/xavidop/tobira/drivers/cosmos"        // registers "cosmos"
	_ "github.com/xavidop/tobira/drivers/d1"            // registers "d1" (scheme d1; Cloudflare D1 REST API)
	_ "github.com/xavidop/tobira/drivers/dynamodb"      // registers "dynamodb"
	_ "github.com/xavidop/tobira/drivers/elasticsearch" // registers "elasticsearch" and "opensearch"
	_ "github.com/xavidop/tobira/drivers/firestore"     // registers "firestore"
	_ "github.com/xavidop/tobira/drivers/influxdb"      // registers "influxdb"
	_ "github.com/xavidop/tobira/drivers/libsql"        // registers "libsql" (scheme libsql; http(s)/ws(s) with driver: libsql)
	_ "github.com/xavidop/tobira/drivers/milvus"        // registers "milvus"
	_ "github.com/xavidop/tobira/drivers/mongodb"       // registers "mongodb"
	_ "github.com/xavidop/tobira/drivers/mssql"         // registers "mssql" (schemes sqlserver, mssql)
	_ "github.com/xavidop/tobira/drivers/mysql"         // registers "mysql", "mariadb" and "singlestore"
	_ "github.com/xavidop/tobira/drivers/oracle"        // registers "oracle"
	_ "github.com/xavidop/tobira/drivers/pinecone"      // registers "pinecone"
	_ "github.com/xavidop/tobira/drivers/postgres"      // registers "postgres", "cockroachdb", "redshift" and "greenplum"
	_ "github.com/xavidop/tobira/drivers/qdrant"        // registers "qdrant"
	_ "github.com/xavidop/tobira/drivers/redis"         // registers "redis"
	_ "github.com/xavidop/tobira/drivers/snowflake"     // registers "snowflake"
	_ "github.com/xavidop/tobira/drivers/sqlite"        // registers "sqlite"
	_ "github.com/xavidop/tobira/drivers/vertica"       // registers "vertica"
	_ "github.com/xavidop/tobira/drivers/weaviate"      // registers "weaviate"
	"github.com/xavidop/tobira/internal/cli"
	"github.com/xavidop/tobira/internal/secrets/keyring"
)

// Set at link time via -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	// tobira logs only through the injected --debug logger. Genkit and the
	// provider SDKs log through slog's default, which would write over the
	// TUI and could carry unredacted request details, so it is silenced.
	// SetDefault also routes the standard log package through this handler,
	// so log.Print* output from any dependency is dropped too, in the CLI as
	// well as the TUI. That is deliberate: tobira itself never uses log, and
	// a dependency's log line has the same screen and redaction problems.
	slog.SetDefault(slog.New(slog.DiscardHandler))
	store := keyring.OS()
	mamori.Register(keyring.NewProvider(store))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		// After the first signal cancels ctx, restore the default handlers so
		// a second Ctrl-C kills a process stuck in a call that ignores ctx.
		// Reset, not just stop: genkit.Init registers its own handler and
		// never removes it, which would swallow the second signal.
		<-ctx.Done()
		stop()
		signal.Reset(os.Interrupt, syscall.SIGTERM)
	}()
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	root := cli.NewRoot(
		cli.BuildInfo{Version: version, Commit: commit, Date: date},
		cli.Streams{In: os.Stdin, Out: os.Stdout, Err: os.Stderr},
		cli.Deps{
			Registry:     driver.Default,
			Getenv:       os.Getenv,
			Environ:      os.Environ,
			Home:         home,
			IsTTY:        isTerminal,
			IsInputTTY:   func() bool { return term.IsTerminal(int(os.Stdin.Fd())) },
			Keyring:      store,
			NewAssistant: newAssistant,
			ServeMCP:     serveMCP,
			NewEmbedder:  newEmbedder,
			CheckAI:      checkAI,
			ReadPassword: func(ctx context.Context) (string, error) {
				return readSecret(ctx, int(os.Stdin.Fd()), os.Stderr, realTermOps)
			},
		},
	)
	code := cli.Execute(ctx, root, os.Args[1:])
	stop()
	os.Exit(code)
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
