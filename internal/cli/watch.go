package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/sshtunnel"
	"github.com/xavidop/tobira/internal/tui/detail"
)

type watchFlags struct {
	format  string
	max     int
	timeout time.Duration
	channel bool
}

// watchColumns are the fields of one event line.
var watchColumns = []driver.Column{
	{Name: "time", Type: driver.TypeTime}, {Name: "op", Type: driver.TypeString},
	{Name: "channel", Type: driver.TypeString}, {Name: "key", Type: driver.TypeObject}, {Name: "payload"},
}

// watchTimeLayout is the time of a table line, in UTC.
const watchTimeLayout = "2006-01-02T15:04:05.000Z"

// watchPayloadWidth is how many columns of a payload a table line shows.
const watchPayloadWidth = 120

func (a *app) newWatchCmd() *cobra.Command {
	var f watchFlags
	cmd := &cobra.Command{
		Use:   "watch <connection|dsn> <namespace.collection|target>",
		Short: "Stream change events to stdout",
		Long: "Stream a connection's change events to stdout, one per line, until Ctrl-C, --max\n" +
			"or --timeout. Watching never writes, so it works on read-only connections.\n\n" +
			"The second argument is a collection as namespace.collection, or a driver-specific\n" +
			"target. A target with a dot is read as namespace.collection unless --channel is given.\n\n" +
			"  PostgreSQL, Greenplum  a LISTEN channel (required: no change streams)    op: notify\n" +
			"  MongoDB                db.collection, or a database (change streams)     op: insert, update, replace, delete, ...\n" +
			"  Redis                  a key pattern (session:*) or db0.prefix            op: insert, update, delete\n" +
			"                         (needs keyspace notifications on the server)\n" +
			"  Amazon DynamoDB        a table with a stream enabled (DynamoDB Streams)  op: insert, update, delete\n" +
			"  Cloud Firestore        a collection path (users/ada/orders) or a         op: insert, update, delete\n" +
			"                         document path (users/ada)\n" +
			"  Azure Cosmos DB        a container, or database.container (change feed)  op: upsert (deletes do not appear)",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 2 {
				return usageError(fmt.Errorf("accepts 2 arguments, received %d", len(args)))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runWatch(cmd, args, f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.format, "format", "jsonl", "output format: jsonl or table")
	fl.IntVar(&f.max, "max", 0, "stop after this many events (0 for no limit)")
	fl.DurationVar(&f.timeout, "timeout", 0, "stop after this long (0 for none); connecting counts")
	fl.BoolVar(&f.channel, "channel", false, "read the target as a channel, database or key pattern even if it has a dot")
	return cmd
}

// watchRequest reads the target argument: namespace.collection, or a
// channel (a target without a dot, one with glob characters, or any target
// with --channel).
func watchRequest(target string, channel bool) driver.WatchRequest {
	ns, name, ok := strings.Cut(target, ".")
	if channel || !ok || ns == "" || name == "" || strings.ContainsAny(target, "*?[") {
		return driver.WatchRequest{Channel: target}
	}
	return driver.WatchRequest{Collection: &driver.CollectionRef{Namespace: ns, Name: name}}
}

func (a *app) runWatch(cmd *cobra.Command, args []string, f watchFlags) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if f.format != "jsonl" && f.format != "table" {
		return usageError(fmt.Errorf("unknown format %q (want jsonl or table)", f.format))
	}
	if f.max < 0 {
		return usageError(fmt.Errorf("--max must not be negative, got %d", f.max))
	}
	if f.timeout < 0 {
		return usageError(fmt.Errorf("--timeout must not be negative, got %s", f.timeout))
	}
	debug, _ := cmd.Flags().GetBool("debug")
	log, closeLog, err := a.openLog(debug)
	if err != nil {
		return usageError(err)
	}
	defer closeLog()
	cfg, tc, err := a.resolve(ctx, cmd, args[0])
	if err != nil {
		return err
	}
	log.Info("watch start", "driver", a.driverName(cfg), "connection", cfg.Name, "format", f.format)

	// The timeout bounds the whole run, connecting included; reaching it
	// while watching is a normal end.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if f.timeout > 0 {
		wctx, cancel = context.WithTimeout(wctx, f.timeout)
		defer cancel()
	}
	cfg.Logger = log
	conn, tun, err := sshtunnel.OpenConn(wctx, a.deps.Registry, cfg, tc)
	if err != nil {
		log.Error("open failed", "kind", driver.KindName(err))
		return scrub(connectError(err), cfg)
	}
	defer func() { _ = tun.Close() }()
	defer func() { _ = conn.Close() }()
	w, ok := driver.As[driver.Watcher](conn)
	if !ok {
		return driver.Wrap(driver.ErrUnsupported, a.driverName(cfg), "watch", errors.New("live tail needs a driver with watch support"))
	}
	events, err := w.Watch(wctx, watchRequest(args[1], f.channel))
	if err != nil {
		log.Error("watch failed", "kind", driver.KindName(err))
		return scrub(err, cfg)
	}
	out := &eventWriter{w: cmd.OutOrStdout(), cfg: cfg, table: f.format == "table"}
	n := 0
	for {
		ev, ok := <-events
		switch {
		case !ok && ctx.Err() != nil: // Ctrl-C
			return fmt.Errorf("watch: %w", ctx.Err())
		case !ok && wctx.Err() != nil: // --timeout
			log.Info("watch done", "events", n, "reason", "timeout")
			return nil
		case !ok:
			return errors.New("watch: the stream ended")
		case ev.Err != nil:
			log.Error("watch failed", "kind", driver.KindName(ev.Err))
			return scrub(ev.Err, cfg)
		}
		if err := out.write(ev); err != nil {
			return err
		}
		n++
		if f.max > 0 && n >= f.max {
			log.Info("watch done", "events", n, "reason", "max")
			return nil
		}
	}
}

// eventWriter writes events as JSON lines or table lines, each redacted
// with the connection's settings and escaped, flushed one by one.
type eventWriter struct {
	w      io.Writer
	cfg    driver.ConnConfig
	table  bool
	header bool
	buf    bytes.Buffer
}

func (e *eventWriter) write(ev driver.Event) error {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	var line string
	if e.table {
		if !e.header {
			e.header = true
			if _, err := fmt.Fprintf(e.w, "%-24s  %-8s  %-24s  %s\n", "TIME", "OP", "KEY", "PAYLOAD"); err != nil {
				return err
			}
		}
		key := ev.Channel
		switch len(ev.Key) {
		case 0:
		case 1:
			key = detail.CellText(ev.Key[0].Value)
		default:
			key = detail.CellText(driver.ObjectValue(ev.Key...))
		}
		payload := query.Truncate(query.EscapeControls(detail.CellText(ev.Payload)), watchPayloadWidth)
		line = ev.Time.UTC().Format(watchTimeLayout) + "  " + padCols(query.Truncate(query.EscapeControls(ev.Op), 8), 8) +
			"  " + padCols(query.Truncate(query.EscapeControls(key), 24), 24) + "  " + payload
		line = query.EscapeControls(driver.RedactMessage(line, e.cfg))
	} else {
		e.buf.Reset()
		rw, err := query.NewRowWriter(&e.buf, query.FormatNDJSON)
		if err != nil {
			return err
		}
		cols := append([]driver.Column(nil), watchColumns...)
		cols[4].Type = ev.Payload.Type()
		_ = rw.Begin(cols)
		_ = rw.Row(driver.Row{driver.Time(ev.Time), driver.String(ev.Op), driver.String(ev.Channel),
			driver.ObjectValue(ev.Key...), ev.Payload})
		if err := rw.End(driver.Stats{}); err != nil {
			return err
		}
		line = driver.RedactMessage(strings.TrimSuffix(e.buf.String(), "\n"), e.cfg)
	}
	_, err := io.WriteString(e.w, line+"\n")
	return err
}

// padCols pads s with spaces to n display columns.
func padCols(s string, n int) string {
	return s + strings.Repeat(" ", max(n-query.DisplayWidth(s), 0))
}
