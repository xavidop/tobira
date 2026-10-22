package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/secrets"
)

// Bounds of `tobira doctor`.
const (
	// doctorPingParallel is how many connections are pinged at once.
	doctorPingParallel = 8
	// doctorDefaultTimeout bounds each ping (--timeout) and each secret
	// ref resolution.
	doctorDefaultTimeout = 5 * time.Second
	// doctorAITimeout bounds the --online provider check.
	doctorAITimeout = 15 * time.Second
)

// The statuses of a doctor check. skip (a check left out on purpose, such
// as a remote secret ref under --no-ping) counts as ok for the exit code.
const (
	statusOK   = "ok"
	statusSkip = "skip"
	statusWarn = "warn"
	statusFail = "fail"
)

// The exit codes of `tobira doctor`: 0 when every check is ok. A usage
// error also exits 2, so that 1 always means warnings.
const (
	doctorExitWarn  = 1
	doctorExitFail  = 2
	doctorExitUsage = 2
)

// doctorUsageError marks err as a doctor usage error (exit 2).
func doctorUsageError(err error) error { return &exitError{code: doctorExitUsage, err: err} }

// maskedSecret replaces a secret value in doctor's output.
const maskedSecret = "xxxxx"

// doctorDetailMax caps a check's detail, in runes, after redaction; a
// longer one is cut and ends with "…".
const doctorDetailMax = 300

// doctorCheck is one line of the report.
type doctorCheck struct {
	Section string `json:"section"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Detail  string `json:"detail"`
}

// doctorReport is `tobira doctor` JSON output.
type doctorReport struct {
	Checks []doctorCheck `json:"checks"`
}

type doctorFlags struct {
	noPing, online bool
	timeout        time.Duration
}

func (a *app) newDoctorCmd() *cobra.Command {
	f := doctorFlags{}
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the config, secret refs, connections, drivers, plugins, AI, MCP and history",
		Long: "Check that tobira is set up correctly. One line per check, each ok, warn or fail:\n" +
			"the config file and its entries (a DSN that carries a password warns), each\n" +
			"connection (pinged read-only, 8 at a time,\n" +
			"--timeout each), each secret ref (ok or fail and its scheme, never the value),\n" +
			"the built-in drivers, the discovered plugins (never started unless --online), the\n" +
			"ai: section (provider, model, key present, embedder), the mcp: section (exposed\n" +
			"names exist, token), the history file (history.path, else the default; opened\n" +
			"read-only, never created, chmodded or migrated: its schema, size and whether it\n" +
			"is writable), and the permissions of the config file,\n" +
			"the history file and the plugins directories.\n\n" +
			"Nothing but the database pings and the secret refs touches the network unless\n" +
			"--online, which also asks the AI provider to list its models and each plugin for\n" +
			"its Info. --no-ping skips the pings and leaves remote refs (any scheme but env,\n" +
			"file, dotenv and keyring) unresolved, reported as skip.\n\n" +
			"A table on a terminal, JSON otherwise. Exit code 0 when every check is ok or\n" +
			"skipped, 1 when some warn, 2 when any fails or on a usage error.",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return doctorUsageError(fmt.Errorf("accepts 0 argument(s), received %d", len(args)))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error { return a.runDoctor(cmd, f) },
	}
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return doctorUsageError(err) })
	fl := cmd.Flags()
	fl.BoolVar(&f.noPing, "no-ping", false, "do not ping the connections nor resolve remote secret refs")
	fl.BoolVar(&f.online, "online", false, "also check the AI provider over the network and start each plugin for its Info")
	fl.DurationVar(&f.timeout, "timeout", doctorDefaultTimeout, "timeout of each connection ping and each secret ref")
	return cmd
}

// doctorRun is one `tobira doctor` run.
type doctorRun struct {
	a   *app
	cmd *cobra.Command
	ctx context.Context
	f   doctorFlags
	log *slog.Logger
	// cfg is the loaded config; nil when it did not load.
	cfg *config.Config
	// refs holds each secret ref's result by its refs-section name, for
	// the AI and MCP sections to reuse.
	refs map[string]doctorCheck
	// res resolves secret refs once per run: the refs phase and the pings
	// share each resolution.
	res secrets.Resolver
	// mu guards seen.
	mu sync.Mutex
	// seen are the secret values doctor read; every output string is
	// masked against them.
	seen []driver.Secret
}

func (r *doctorRun) sawSecret(s driver.Secret) {
	if s.IsZero() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, s)
}

func (a *app) runDoctor(cmd *cobra.Command, f doctorFlags) error {
	if f.timeout <= 0 {
		return doctorUsageError(fmt.Errorf("--timeout must be positive, got %s", f.timeout))
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	debug, _ := cmd.Flags().GetBool("debug")
	log, closeLog, err := a.openLog(debug)
	if err != nil {
		return doctorUsageError(err)
	}
	defer closeLog()
	r := &doctorRun{a: a, cmd: cmd, ctx: ctx, f: f, log: log, refs: map[string]doctorCheck{}, res: a.deps.Resolver.Cached()}

	var checks []doctorCheck
	checks = append(checks, r.checkConfig()...)
	perms := r.checkPermissions() // needs the config for history.path; reported last
	refs := r.checkRefs()
	checks = append(checks, r.checkConnections()...)
	checks = append(checks, refs...)
	checks = append(checks, r.checkDrivers()...)
	checks = append(checks, r.checkPlugins()...)
	checks = append(checks, r.checkAI()...)
	checks = append(checks, r.checkMCP()...)
	checks = append(checks, r.checkHistory()...)
	checks = append(checks, perms...)
	for i := range checks {
		checks[i] = r.clean(checks[i])
	}
	if err := a.writeDoctor(cmd.OutOrStdout(), checks); err != nil {
		return err
	}
	return doctorExit(checks)
}

// clean redacts and escapes every field of c: credentials in URLs and
// key=value pairs, and every secret value doctor read, which is masked
// exactly whatever its length (RedactMessage alone leaves values shorter
// than 4 bytes in free text). Over-masking a short value is preferred to
// leaking it.
func (r *doctorRun) clean(c doctorCheck) doctorCheck {
	scrub := func(s string) string {
		s = driver.RedactMessage(s, driver.ConnConfig{})
		for _, sec := range r.seen {
			s = driver.RedactMessage(s, driver.ConnConfig{Password: sec})
			if v := sec.Reveal(); v != "" {
				s = strings.ReplaceAll(s, v, maskedSecret)
			}
		}
		return s
	}
	return doctorCheck{
		Section: c.Section,
		Name:    query.EscapeControls(scrub(c.Name)),
		Status:  c.Status,
		Detail:  query.EscapeControls(capRunes(scrub(c.Detail), doctorDetailMax)),
	}
}

// errLine is err's message on one line, so a driver that puts the cause on
// the following lines (pgx does) keeps it: each line is trimmed, runs of
// spaces and tabs fold to one space, and the lines are joined with " — ",
// or a space after a line that ends with a colon. clean redacts, caps and
// escapes the result.
func errLine(err error) string {
	var b strings.Builder
	for _, l := range strings.FieldsFunc(err.Error(), func(r rune) bool { return r == '\n' || r == '\r' }) {
		l = strings.Join(strings.Fields(l), " ")
		if l == "" {
			continue
		}
		if b.Len() > 0 {
			if strings.HasSuffix(b.String(), ":") {
				b.WriteString(" ")
			} else {
				b.WriteString(" — ")
			}
		}
		b.WriteString(l)
	}
	return b.String()
}

// capRunes cuts s to n runes and appends "…" when it is longer.
func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strings.TrimRight(string([]rune(s)[:n]), " ") + "…"
}

func (a *app) writeDoctor(out io.Writer, checks []doctorCheck) error {
	if a.deps.IsTTY == nil || !a.deps.IsTTY(a.streams.Out) {
		if checks == nil {
			checks = []doctorCheck{}
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(doctorReport{Checks: checks})
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SECTION\tNAME\tSTATUS\tDETAIL")
	counts := map[string]int{}
	for _, c := range checks {
		counts[c.Status]++
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", c.Section, c.Name, c.Status, c.Detail)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "\n%d ok, %d skip, %d warn, %d fail\n", counts[statusOK], counts[statusSkip], counts[statusWarn], counts[statusFail])
	return err
}

// doctorExit is nil when every check is ok, else a silent exit error with
// code 1 (warnings only) or 2 (a failure).
func doctorExit(checks []doctorCheck) error {
	warns, fails := 0, 0
	for _, c := range checks {
		switch c.Status {
		case statusWarn:
			warns++
		case statusFail:
			fails++
		}
	}
	switch {
	case fails > 0:
		return &exitError{code: doctorExitFail, err: fmt.Errorf("doctor: %d failed, %d warned", fails, warns), silent: true}
	case warns > 0:
		return &exitError{code: doctorExitWarn, err: fmt.Errorf("doctor: %d warned", warns), silent: true}
	}
	return nil
}

// within runs probe in its own goroutine and waits for it or for ctx,
// whichever ends first. When ctx ends first it returns ctx.Err() and
// abandons the probe: a driver or secret store that ignores ctx (a
// blocking C call, a prompt) keeps its goroutine until it returns. That
// leak is accepted, since doctor is a short-lived process; the buffered
// channel lets the goroutine exit then instead of blocking forever.
func within[T any](ctx context.Context, probe func(context.Context) (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := probe(ctx)
		ch <- result{v, err}
	}()
	select {
	case res := <-ch:
		return res.v, res.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// forEachLimit calls fn(i) for every i below n, at most limit at a time,
// and returns when all have returned.
func forEachLimit(n, limit int, fn func(i int)) {
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range n {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			fn(i)
		})
	}
	wg.Wait()
}
