package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/history"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/session"
)

type askFlags struct {
	conn       string
	collection string
	answer     bool
	sendRows   bool
	format     string
	run        bool
	limit      int
	timeout    time.Duration
}

// aiDocsURL is where an unconfigured `tobira ask` sends the user.
// maxQuestionBytes caps a question read from stdin.
const maxQuestionBytes = 64 * 1024

const aiDocsURL = "https://xavidop.github.io/tobira/docs/ai/"

func (a *app) newAskCmd() *cobra.Command {
	var f askFlags
	cmd := &cobra.Command{
		Use:   "ask [connection] <question>",
		Short: "Ask the AI assistant about a database",
		Long: "Ask a question in plain language. By default tobira prints the query the assistant\n" +
			"proposes and why; nothing is run. With --run the proposed query is then run\n" +
			"read-only, as `tobira query --read-only` would run it, and its result printed.\n" +
			"With --answer the assistant runs read-only queries itself and prints its answer\n" +
			"and the queries it ran.\n\n" +
			"The question is the last argument, or \"-\" to read it from stdin. The connection\n" +
			"(a configured name or a DSN) is the first argument or --conn; without either the\n" +
			"only configured connection is used. The assistant's tools reach only that\n" +
			"connection. Row data obeys ai.send_rows: with ask (the default) rows are withheld\n" +
			"from the model unless --send-rows is given, with always they are sent without it,\n" +
			"and with never they are always withheld (--send-rows is refused). The assistant\n" +
			"is configured by the ai: section of the config file.",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) < 1 || len(args) > 2 {
				return usageError(fmt.Errorf("accepts 1 or 2 arguments ([connection] and the question, or - for stdin), received %d", len(args)))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			question := args[len(args)-1]
			if len(args) == 2 {
				if cmd.Flags().Changed("conn") {
					return usageError(errors.New("give the connection either as the first argument or with --conn, not both"))
				}
				f.conn = args[0]
			}
			return a.runAsk(cmd, question, f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.conn, "conn", "", "connection name or DSN, the same as the first argument (default: the only configured connection)")
	fl.StringVar(&f.collection, "collection", "", `collection to focus on: ns.name, or name in the default namespace ("a.b"."c.d" quotes dots)`)
	fl.BoolVar(&f.answer, "answer", false, "let the assistant run read-only queries and print its answer")
	fl.BoolVar(&f.sendRows, "send-rows", false, "allow row data to be sent to the model for this run")
	fl.StringVar(&f.format, "format", "text", "output format: text or json; with --run, the result's format: table, json, ndjson or csv (default table on a terminal, else json)")
	fl.BoolVar(&f.run, "run", false, "run the proposed query read-only and print its result, as tobira query does")
	fl.IntVar(&f.limit, "limit", 1000, "with --run: maximum rows to return (0 for no limit)")
	fl.DurationVar(&f.timeout, "timeout", 30*time.Second, "with --run: timeout for the query, connecting included (0 for none)")
	return cmd
}

// askRunFormat checks the flags that --run governs and settles the format
// of its result: the query formats, by default as for tobira query.
func (a *app) askRunFormat(cmd *cobra.Command, f askFlags) (query.Format, error) {
	fl := cmd.Flags()
	if !f.run {
		for _, name := range []string{"limit", "timeout"} {
			if fl.Changed(name) {
				return "", usageError(fmt.Errorf("--%s applies only with --run", name))
			}
		}
		if f.format != "text" && f.format != "json" {
			return "", usageError(fmt.Errorf("unknown format %q (valid formats: text, json)", f.format))
		}
		return "", nil
	}
	if f.answer {
		return "", usageError(errors.New("--run cannot be combined with --answer: --answer runs its own read-only queries"))
	}
	if f.limit < 0 {
		return "", usageError(fmt.Errorf("--limit must not be negative, got %d", f.limit))
	}
	if f.timeout < 0 {
		return "", usageError(fmt.Errorf("--timeout must not be negative, got %s", f.timeout))
	}
	if !fl.Changed("format") {
		return a.format("")
	}
	return a.format(f.format)
}

func (a *app) runAsk(cmd *cobra.Command, arg string, f askFlags) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	question, err := a.askQuestion(arg)
	if err != nil {
		return err
	}
	runFormat, err := a.askRunFormat(cmd, f)
	if err != nil {
		return err
	}
	var collNS, collName string
	if f.collection != "" {
		if collNS, collName, err = parseCollection(f.collection); err != nil {
			return usageError(fmt.Errorf("--collection %w", err))
		}
	}
	debug, _ := cmd.Flags().GetBool("debug")
	log, closeLog, err := a.openLog(debug)
	if err != nil {
		return usageError(err)
	}
	defer closeLog()
	cfg, err := a.loadConfig(cmd)
	if err != nil {
		return err
	}
	if cfg.AI == nil || a.deps.NewAssistant == nil {
		return a.notConfigured(cmd)
	}
	if f.sendRows && cfg.AI.SendRows == "never" {
		// Refused by policy: exit 4, like a write refused on a read-only
		// connection.
		return &exitError{code: exitDenied, err: errors.New("ai.send_rows is never; edit the config to allow rows")}
	}
	// With --run the session is read-only at the protocol level, as
	// tobira query --read-only is: the proposal runs on a connection opened
	// read-only, not only with read-only calls. Without --run nothing here
	// can write either (the CLI never offers execute).
	mgr, _ := a.newManager(ctx, cfg, f.run, log)
	defer func() { _ = mgr.CloseAll() }()
	connName, err := a.askConnection(cfg, mgr, f.conn)
	if err != nil {
		return err
	}
	// A plugin connection's real Info, before the assistant's tools read
	// its capabilities.
	unavailable, err := a.resolvePlugins(ctx, mgr, []string{connName}, log)
	if err != nil {
		return err // cancelled: the usual exit for an interrupt
	}
	if why, ok := unavailable[connName]; ok {
		return &exitError{code: exitConnection, err: errUnavailableConn(connName, why)}
	}
	hist := a.openHistory(ctx, log, cfg)
	defer func() { _ = hist.Close() }()
	asst, err := a.buildAssistant(ctx, cfg, mgr, hist, log)
	if err != nil {
		return askFailure(err, exitUsage)
	}
	if asst == nil {
		return a.notConfigured(cmd)
	}
	scope := assistant.Scope{Conn: connName}
	if collName != "" {
		if collNS == "" {
			if collNS, err = defaultNamespace(ctx, mgr, connName); err != nil {
				return err
			}
		}
		scope.Namespace, scope.Collection = collNS, collName
	}

	p := &progress{w: a.streams.Err, on: debug || a.isTTY(a.streams.Err)}
	p.thinking(asst.Model())
	ev := assistant.Events{
		ToolCall: p.toolCall,
		ConsentRows: func(context.Context, assistant.ConsentRequest) (assistant.Consent, error) {
			if f.sendRows {
				return assistant.ConsentSession, nil
			}
			return assistant.ConsentNo, nil
		},
	}
	log.Info("ask start", "connection", connName, "answer", f.answer, "send_rows", f.sendRows)
	out := askOutput{w: a.streams.Out, json: f.format == "json", dim: a.isTTY(a.streams.Out)}
	if f.answer {
		res, err := asst.Answer(ctx, assistant.AnswerRequest{Scope: scope, Question: question}, ev)
		if err != nil {
			return askFailure(redactedWith(mgr, connName, err), exitQuery)
		}
		return out.answer(res)
	}
	res, err := asst.Ask(ctx, assistant.AskRequest{Scope: scope, Question: question}, ev)
	if err != nil {
		return askFailure(redactedWith(mgr, connName, err), exitQuery)
	}
	if !f.run {
		return out.ask(res)
	}
	// The proposal goes to stderr, so stdout carries the result alone.
	if err := (askOutput{w: a.streams.Err, dim: a.isTTY(a.streams.Err)}).ask(res); err != nil {
		return err
	}
	return a.runProposal(ctx, mgr, connName, res.Query, runFormat, f, hist, log)
}

// runProposal runs the proposed query as `tobira query --read-only` would:
// read-only whatever the connection allows, with --limit and --timeout,
// recorded in the history as a CLI run, and printed in format. Its errors
// map to the same exit codes.
func (a *app) runProposal(ctx context.Context, mgr *session.Manager, connName, text string, format query.Format,
	f askFlags, hist *history.Store, log *slog.Logger) error {
	if f.timeout > 0 {
		// The timeout also bounds connecting, not just the query.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.timeout)
		defer cancel()
	}
	conn, err := mgr.Open(ctx, connName)
	if err != nil {
		log.Error("open failed", "kind", driver.KindName(err))
		return connectError(redactedWith(mgr, connName, err))
	}
	drv := mgr.State(connName).Info.Name
	log.Info("query start", "driver", drv, "connection", connName, "format", string(format), "read_only", true, "source", "ask --run")
	start := time.Now()
	res, err := query.Run(ctx, conn, text, query.Options{Limit: f.limit, Timeout: f.timeout, ReadOnly: true, Driver: drv})
	entry := history.Entry{At: start, Conn: connName, Source: history.SourceCLI, Query: mgr.Redact(connName, text), Took: time.Since(start)}
	if err != nil {
		log.Error("query failed", "kind", driver.KindName(err), "duration", time.Since(start).String())
		a.recordHistory(ctx, log, hist, entry, driver.ConnConfig{})
		return redactedWith(mgr, connName, err)
	}
	entry.OK = true
	entry.Rows = int64(len(res.Rows))
	if len(res.Columns) == 0 && res.Stats.RowsAffected > 0 {
		entry.Rows = res.Stats.RowsAffected
	}
	a.recordHistory(ctx, log, hist, entry, driver.ConnConfig{})
	log.Info("query done", "rows", len(res.Rows), "duration", time.Since(start).String())
	return query.Write(a.streams.Out, format, res)
}

func (a *app) isTTY(w io.Writer) bool { return a.deps.IsTTY != nil && a.deps.IsTTY(w) }

// askQuestion returns the question: the argument, or stdin for "-".
func (a *app) askQuestion(arg string) (string, error) {
	if arg == "-" {
		if a.deps.IsInputTTY != nil && a.deps.IsInputTTY() && a.streams.Err != nil {
			_, _ = fmt.Fprintln(a.streams.Err, "reading the question from stdin; end with Ctrl-D")
		}
		data, err := io.ReadAll(io.LimitReader(a.streams.In, maxQuestionBytes+1))
		if err != nil {
			return "", usageError(fmt.Errorf("read question: %w", err))
		}
		if len(data) > maxQuestionBytes {
			return "", usageError(errors.New("the question on stdin is longer than 64 KiB"))
		}
		arg = string(data)
	}
	q := strings.TrimSpace(arg)
	if q == "" {
		return "", usageError(errors.New("the question is empty"))
	}
	return q, nil
}

func (a *app) notConfigured(cmd *cobra.Command) error {
	where := "the config file"
	if p := a.configPath(cmd); p != "" {
		where = query.EscapeControls(p)
	}
	return usageError(fmt.Errorf("%w: add an ai: section to %s (see %s)", assistant.ErrNotConfigured, where, aiDocsURL))
}

// askConnection settles the connection name the assistant is scoped to:
// --conn (a configured name or a DSN), else the only configured connection.
func (a *app) askConnection(cfg *config.Config, mgr *session.Manager, arg string) (string, error) {
	switch {
	case arg == "":
		names := make([]string, len(cfg.Connections))
		for i, c := range cfg.Connections {
			names[i] = query.EscapeControls(driver.RedactMessage(c.Name, driver.ConnConfig{}))
		}
		switch len(names) {
		case 1:
			return cfg.Connections[0].Name, nil
		case 0:
			return "", usageError(errors.New("no connection is configured: add one, or pass --conn with a DSN"))
		}
		return "", usageError(fmt.Errorf("several connections are configured: choose one with --conn (%s)", strings.Join(names, ", ")))
	case isDSN(arg):
		name, err := mgr.AddDSN(arg)
		if err != nil {
			return "", usageError(scrubDSN(err, arg))
		}
		return name, nil
	}
	if _, ok := cfg.Find(arg); !ok {
		return "", unknownConnection(cfg, arg)
	}
	return arg, nil
}

// defaultNamespace is the connection's default namespace, for a --collection
// given as a bare name.
func defaultNamespace(ctx context.Context, mgr *session.Manager, name string) (string, error) {
	fail := func(err error) error {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return usageError(fmt.Errorf("--collection: find the default namespace of %s: %s", name, mgr.Redact(name, err.Error())))
	}
	conn, err := mgr.Open(ctx, name)
	if err != nil {
		// As in `tobira query`: a connection that cannot be opened is exit 2.
		return "", connectError(redactedWith(mgr, name, err))
	}
	nss, err := conn.Namespaces(ctx)
	if err != nil {
		return "", fail(err)
	}
	for _, ns := range nss {
		if ns.Default {
			return ns.Name, nil
		}
	}
	return "", usageError(fmt.Errorf("--collection: %s has no default namespace: give ns.name", name))
}

// redactedWith returns err with its message passed through the connection's
// redactor; the original stays reachable for errors.Is and errors.As.
func redactedWith(mgr *session.Manager, name string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &scrubbedError{msg: mgr.Redact(name, err.Error()), err: err}
}

// parseCollection reads --collection: "name", "ns.name", or either part in
// double quotes (a doubled quote inside stands for one) so a dot can be part
// of a name. A bare name has an empty namespace. The error reads as the
// rest of a sentence after "--collection".
func parseCollection(s string) (ns, name string, err error) {
	var parts []string
	for i := 0; ; {
		var part string
		if i < len(s) && s[i] == '"' {
			var b strings.Builder
			i++
			for closed := false; !closed; {
				switch {
				case i >= len(s):
					return "", "", errors.New("has an unterminated quote")
				case s[i] == '"' && i+1 < len(s) && s[i+1] == '"':
					b.WriteByte('"')
					i += 2
				case s[i] == '"':
					i++
					closed = true
				default:
					b.WriteByte(s[i])
					i++
				}
			}
			part = b.String()
			if i < len(s) && s[i] != '.' {
				return "", "", errors.New(`has text after a closing quote: separate names with "."`)
			}
		} else {
			end := strings.IndexByte(s[i:], '.')
			if end < 0 {
				end = len(s) - i
			}
			part = s[i : i+end]
			i += end
			if strings.Contains(part, `"`) {
				return "", "", errors.New("has a quote inside a name: quote the whole name")
			}
		}
		if part == "" {
			return "", "", errors.New("has an empty name")
		}
		parts = append(parts, part)
		if i >= len(s) {
			break
		}
		i++ // the "."
		if i >= len(s) {
			return "", "", errors.New("has an empty name")
		}
	}
	switch len(parts) {
	case 1:
		return "", parts[0], nil
	case 2:
		return parts[0], parts[1], nil
	}
	return "", "", errors.New(`has too many parts: want name or ns.name, with "quotes" around a name that contains a dot`)
}

// askFailure maps an assistant error to the exit code: 1 for a bad scope or
// no AI, 2 when the provider cannot be used, 3 for a model error (fallback is
// the code of an error with no known kind). Cancellation keeps its own code.
func askFailure(err error, fallback int) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, assistant.ErrScope), errors.Is(err, assistant.ErrNotConfigured):
		return &exitError{code: exitUsage, err: err}
	case errors.Is(err, assistant.ErrAuth), errors.Is(err, assistant.ErrUnavailable), errors.Is(err, assistant.ErrRateLimit):
		return &exitError{code: exitConnection, err: err}
	case errors.Is(err, assistant.ErrProvider):
		return &exitError{code: exitQuery, err: err}
	}
	return &exitError{code: fallback, err: err}
}

// progress writes the status lines of a turn to stderr. It is off unless
// stderr is a terminal (or --debug), so a pipe sees nothing.
type progress struct {
	mu sync.Mutex
	w  io.Writer
	on bool
}

func (p *progress) line(format string, args ...any) {
	if !p.on || p.w == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = fmt.Fprintf(p.w, format+"\n", args...)
}

func (p *progress) thinking(model string) {
	p.line("thinking… (%s)", query.EscapeControls(model))
}

func (p *progress) toolCall(tc assistant.ToolCall) {
	s := query.EscapeControls(strings.TrimSpace(tc.Name + " " + tc.Input))
	switch {
	case tc.Err != "":
		s += ": error: " + query.EscapeControls(tc.Err)
	case tc.Rows >= 0:
		s += ": " + rowCount(tc.Rows)
	}
	p.line("▸ %s", s)
}

func rowCount(n int) string {
	if n == 1 {
		return "1 row"
	}
	return fmt.Sprintf("%d rows", n)
}

// askOutput prints a turn's result as text or JSON.
type askOutput struct {
	w    io.Writer
	json bool
	dim  bool
}

type usageJSON struct {
	Model        string `json:"model"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	LatencyMS    int64  `json:"latency_ms"`
}

type ranJSON struct {
	Conn  string `json:"conn,omitempty"`
	Query string `json:"query"`
	Rows  int    `json:"rows"`
	Error string `json:"error,omitempty"`
}

type askJSON struct {
	Mode        string     `json:"mode"`
	Query       string     `json:"query,omitempty"`
	Explanation string     `json:"explanation,omitempty"`
	Markdown    string     `json:"markdown,omitempty"`
	Ran         *[]ranJSON `json:"ran,omitempty"`
	Usage       usageJSON  `json:"usage"`
}

func toUsageJSON(u assistant.Usage) usageJSON {
	return usageJSON{Model: u.Model, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, LatencyMS: u.Latency.Milliseconds()}
}

func (o askOutput) write(v askJSON) error {
	enc := json.NewEncoder(o.w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func (o askOutput) ask(r assistant.AskResult) error {
	if o.json {
		return o.write(askJSON{Mode: "ask", Query: r.Query, Explanation: r.Explanation, Usage: toUsageJSON(r.Usage)})
	}
	var b strings.Builder
	q := strings.TrimRight(query.EscapeControlsMultiline(r.Query), "\n")
	fence := strings.Repeat("`", max(3, longestRun(q, '`')+1))
	b.WriteString(fence + query.EscapeControls(r.Dialect) + "\n" + q + "\n" + fence + "\n")
	if e := strings.TrimSpace(query.EscapeControlsMultiline(r.Explanation)); e != "" {
		b.WriteString("\n" + e + "\n")
	}
	return o.finish(&b, r.Usage)
}

func (o askOutput) answer(r assistant.AnswerResult) error {
	if o.json {
		ran := make([]ranJSON, len(r.Ran))
		for i, q := range r.Ran {
			ran[i] = ranJSON{Conn: q.Conn, Query: q.Query, Rows: q.Rows, Error: q.Err}
		}
		return o.write(askJSON{Mode: "answer", Markdown: r.Markdown, Ran: &ran, Usage: toUsageJSON(r.Usage)})
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(query.EscapeControlsMultiline(r.Markdown)) + "\n")
	if len(r.Ran) > 0 {
		b.WriteString("\nQueries run:\n")
		for _, q := range r.Ran {
			b.WriteString("  " + query.EscapeControls(q.Query))
			switch {
			case q.Err != "":
				b.WriteString(" (error: " + query.EscapeControls(q.Err) + ")")
			case q.Rows >= 0:
				b.WriteString(" (" + rowCount(q.Rows) + ")")
			}
			b.WriteString("\n")
		}
	}
	return o.finish(&b, r.Usage)
}

// finish appends the footer and writes the text.
func (o askOutput) finish(b *strings.Builder, u assistant.Usage) error {
	footer := usageFooter(u)
	if o.dim {
		footer = "\x1b[2m" + footer + "\x1b[0m"
	}
	b.WriteString("\n" + footer + "\n")
	_, err := io.WriteString(o.w, b.String())
	return err
}

// usageFooter is "model · 1.2k in / 340 out · 2.1s".
func usageFooter(u assistant.Usage) string {
	return fmt.Sprintf("%s · %s in / %s out · %s", query.EscapeControls(u.Model), tokenCount(u.InputTokens), tokenCount(u.OutputTokens), latency(u.Latency))
}

func tokenCount(n int) string {
	if n < 1000 {
		return fmt.Sprint(n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

func latency(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// longestRun is the length of the longest run of c in s.
func longestRun(s string, c byte) int {
	best, cur := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			cur++
			best = max(best, cur)
		} else {
			cur = 0
		}
	}
	return best
}
