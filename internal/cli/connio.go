package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/connio"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/secrets/keyring"
)

func (a *app) newConnExportCmd() *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "export [names...]",
		Short: "Print connections as YAML or JSON, without passwords",
		Long: "Print the named connections (all of them by default) to stdout in the config's\n" +
			"connections: shape, as YAML or, with --format json, JSON. Password references\n" +
			"(env:, file://, keyring: ...) are kept: they are not secrets. A literal password,\n" +
			"a password inside a DSN and an option that looks like a secret are left out,\n" +
			"with a note on stderr. `tobira connections import --from file` reads the result.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "yaml" && format != "json" {
				return usageError(fmt.Errorf("--format must be yaml or json, not %q", format))
			}
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			conns := cfg.Connections
			if len(args) > 0 {
				conns = nil
				seen := map[string]bool{}
				for _, name := range args {
					c, ok := cfg.Find(name)
					if !ok {
						return unknownConnection(cfg, name)
					}
					if !seen[name] {
						seen[name] = true
						conns = append(conns, c)
					}
				}
			} else if n := len(cfg.Broken); n > 0 {
				lines := make([]string, n)
				for i, b := range cfg.Broken {
					lines[i] = strconv.Itoa(b.Line)
				}
				a.note(fmt.Sprintf("%s left out (line %s)", plural(int64(n), "broken entry", "broken entries"), strings.Join(lines, ", ")))
			}
			out, notes := connio.Export(conns)
			for _, n := range notes {
				a.note(n)
			}
			var b []byte
			if format == "json" {
				b, err = connio.MarshalJSON(out)
			} else {
				b, err = config.MarshalConnections(out)
			}
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(b)
			return err
		},
	}
	cmd.Flags().StringVar(&format, "format", "yaml", "output format: yaml or json")
	return cmd
}

// note prints one "note:" line on stderr, redacted and escaped.
func (a *app) note(s string) {
	_, _ = fmt.Fprintf(a.streams.Err, "note: %s\n", query.EscapeControls(driver.RedactMessage(s, driver.ConnConfig{})))
}

type importFlags struct {
	from, suffix                   string
	apply, noPasswords, keyringYes bool
}

var importSources = []string{"dbeaver", "pgpass", "tableplus", "file"}

func (a *app) newConnImportCmd() *cobra.Command {
	var f importFlags
	cmd := &cobra.Command{
		Use:   "import --from dbeaver|pgpass|tableplus|file [path]",
		Short: "Import connections from DBeaver, pgpass, TablePlus or an export file",
		Long: "Read connections from another tool and add them to the config file.\n\n" +
			"  dbeaver    DBeaver's data-sources.json. Passwords only from plain\n" +
			"             configuration fields; the encrypted credentials-config.json is never read.\n" +
			"  pgpass     a libpq password file (default $PGPASSFILE or ~/.pgpass); it must not be\n" +
			"             group or world accessible, as libpq requires.\n" +
			"  tableplus  TablePlus's Connections.plist (default: its macOS location). Its\n" +
			"             passwords live in the keychain, which is never read.\n" +
			"  file       the YAML or JSON `tobira connections export` prints.\n\n" +
			"Without --apply this prints the plan (add, rename, skip with the reason) and writes\n" +
			"nothing. With --apply the new connections are added in one write that keeps the\n" +
			"file's comments, and fails if the file changed meanwhile. A name already taken gets\n" +
			"--rename-suffix, then a number. A password found in the source is never written to\n" +
			"the config: with --apply it is stored in the OS keyring (keyring:<name>) without\n" +
			"asking when --keyring-yes is given, terminal or not; otherwise on a terminal you\n" +
			"are asked per connection, and without one it is skipped. --no-passwords skips them.\n" +
			"A connection whose driver this build does not have is skipped.",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 1 {
				return usageError(fmt.Errorf("accepts at most 1 argument, received %d", len(args)))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error { return a.runImport(cmd, f, args) },
	}
	fl := cmd.Flags()
	fl.StringVar(&f.from, "from", "", "source format: "+strings.Join(importSources, ", "))
	fl.BoolVar(&f.apply, "apply", false, "write the connections (default: print the plan only)")
	fl.StringVar(&f.suffix, "rename-suffix", "-imported", "suffix for an imported name that is already taken")
	fl.BoolVar(&f.noPasswords, "no-passwords", false, "ignore the passwords the source holds")
	fl.BoolVar(&f.keyringYes, "keyring-yes", false, "store found passwords in the OS keyring without asking (even on a terminal)")
	return cmd
}

// importPath returns the file to read: the argument, or the source's
// default location.
func (a *app) importPath(from string, args []string) (string, error) {
	if len(args) == 1 {
		return args[0], nil
	}
	switch from {
	case "pgpass":
		if p := a.getenv("PGPASSFILE"); p != "" {
			return p, nil
		}
		if a.deps.Home != "" {
			return filepath.Join(a.deps.Home, ".pgpass"), nil
		}
	case "tableplus":
		if a.deps.Home != "" {
			return filepath.Join(a.deps.Home, connio.TablePlusPath), nil
		}
	}
	return "", usageError(fmt.Errorf("import --from %s needs the path of the file to read", from))
}

// maxImportSize caps what an import reads, so that /dev/zero or a huge file
// cannot exhaust memory.
const maxImportSize = 16 << 20

func (a *app) readImport(from, path string) ([]connio.Candidate, error) {
	fail := func(err error) error { return usageError(fmt.Errorf("import from %s: %w", from, err)) }
	errIrregular := errors.New("not a regular file")
	errHuge := errors.New("larger than 16 MiB")
	// Stat before opening: opening a FIFO would block.
	pre, err := os.Stat(path)
	if err != nil {
		return nil, fail(err)
	}
	if !pre.Mode().IsRegular() {
		return nil, fail(errIrregular)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fail(err)
	}
	defer func() { _ = f.Close() }()
	// Every check is on the file as opened, so it cannot be swapped between
	// the check and the read.
	fi, err := f.Stat()
	if err != nil {
		return nil, fail(err)
	}
	if !fi.Mode().IsRegular() || !os.SameFile(pre, fi) {
		return nil, fail(errIrregular)
	}
	if fi.Size() > maxImportSize {
		return nil, fail(errHuge)
	}
	if from == "pgpass" && runtime.GOOS != "windows" {
		if err := connio.CheckPgpassMode(fi.Mode()); err != nil {
			return nil, fail(err)
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, maxImportSize+1))
	if err != nil {
		return nil, fail(err)
	}
	if len(data) > maxImportSize {
		return nil, fail(errHuge)
	}
	var cands []connio.Candidate
	switch from {
	case "dbeaver":
		cands, err = connio.ParseDBeaver(data)
	case "pgpass":
		cands, err = connio.ParsePgpass(data)
	case "tableplus":
		cands, err = connio.ParseTablePlus(data)
	case "file":
		cands, err = connio.ParseFile(data)
	}
	if err != nil {
		return nil, fail(err)
	}
	return cands, nil
}

// pwMode is what happens to the password found for one plan entry.
type pwMode int

const (
	pwNone  pwMode = iota // no password found
	pwSkip                // found, not stored (pwWhy says why)
	pwAsk                 // ask on --apply
	pwStore               // store on --apply without asking
)

type importItem struct {
	connio.Entry
	mode pwMode
	why  string
}

func (a *app) runImport(cmd *cobra.Command, f importFlags, args []string) error {
	if f.from == "" {
		return usageError(errors.New("--from is required: " + strings.Join(importSources, ", ")))
	}
	if !slices.Contains(importSources, f.from) {
		return usageError(fmt.Errorf("--from must be one of %s, not %q", strings.Join(importSources, ", "), f.from))
	}
	if f.suffix == "" || config.ValidateName("x"+f.suffix) != nil {
		return usageError(errors.New("--rename-suffix must be non-empty and use only letters, digits, '.', '_' and '-'"))
	}
	path, err := a.importPath(f.from, args)
	if err != nil {
		return err
	}
	cands, err := a.readImport(f.from, path)
	if err != nil {
		return err
	}
	wc, err := a.writeCtx(cmd)
	if err != nil {
		return err
	}
	var taken []string
	for _, c := range wc.cfg.Connections {
		taken = append(taken, c.Name)
	}
	for _, b := range wc.cfg.Broken {
		if b.Name != "" {
			taken = append(taken, b.Name)
		}
	}
	// An export may name a plugin driver not installed here: keep it, with a
	// note. Other tools' entries map to built-in drivers, so skip them.
	plan := connio.Plan(cands, taken, f.suffix, a.driverAvailable, f.from == "file")
	items := a.passwordModes(wc.cfg, plan, f)

	out := cmd.OutOrStdout()
	a.printPlan(out, f.from, items)
	adds := 0
	for _, it := range items {
		if it.Action != connio.ActionSkip {
			adds++
		}
	}
	if !f.apply {
		_, _ = fmt.Fprintln(out, "dry run: nothing written. Re-run with --apply to write.")
		return nil
	}
	if adds == 0 {
		_, _ = fmt.Fprintln(out, "nothing to import")
		return nil
	}
	return a.applyImport(cmd, wc, items)
}

// driverAvailable reports whether this build (built-in drivers and
// registered plugins) has the driver c names, or the one its DSN scheme
// picks.
func (a *app) driverAvailable(c config.Connection) bool {
	if c.Driver != "" {
		_, ok := a.deps.Registry.Lookup(c.Driver)
		return ok
	}
	_, ok := a.deps.Registry.ForScheme(driver.Scheme(c.DSN))
	return ok
}

// passwordModes decides, without touching the keyring, what happens to each
// found password.
func (a *app) passwordModes(cfg *config.Config, plan []connio.Entry, f importFlags) []importItem {
	// Refs the config and the batch already use: storing keyring:<name>
	// must not change another connection's password.
	refUser := map[string]string{}
	for _, c := range cfg.Connections {
		refUser[c.Password] = c.Name
	}
	for _, b := range cfg.Broken {
		refUser[b.Partial.Password] = b.Name
	}
	for _, e := range plan {
		if e.Action != connio.ActionSkip && e.Conn.Password != "" {
			refUser[e.Conn.Password] = e.Conn.Name
		}
	}
	items := make([]importItem, len(plan))
	for i, e := range plan {
		it := importItem{Entry: e}
		if e.Action != connio.ActionSkip && !e.Password.IsZero() {
			ref := keyring.Ref(e.Conn.Name)
			switch user, shared := refUser[ref]; {
			case f.noPasswords:
				it.mode, it.why = pwSkip, "--no-passwords"
			case e.Conn.Password != "":
				it.mode, it.why = pwSkip, "the entry already has a password reference"
			case a.deps.Keyring == nil:
				it.mode, it.why = pwSkip, "no OS keyring in this build"
			case shared:
				it.mode, it.why = pwSkip, ref+" is used by connection "+user
			case f.keyringYes: // explicit consent: never asks, even on a terminal
				it.mode = pwStore
			case a.deps.IsInputTTY != nil && a.deps.IsInputTTY():
				it.mode = pwAsk
			default:
				it.mode, it.why = pwSkip, "no terminal to ask; pass --keyring-yes to store it"
			}
		}
		items[i] = it
	}
	return items
}

func (a *app) printPlan(w io.Writer, from string, items []importItem) {
	esc := func(s string) string { return query.EscapeControls(driver.RedactMessage(s, driver.ConnConfig{})) }
	switch from {
	case "dbeaver":
		_, _ = fmt.Fprintln(w, "note: DBeaver's encrypted credentials-config.json is not read; passwords come only from plain configuration fields")
	case "tableplus":
		_, _ = fmt.Fprintln(w, "note: TablePlus keeps passwords in the macOS keychain, which tobira does not read; use tobira connections set-password")
	}
	adds, renames, skips := 0, 0, 0
	for _, it := range items {
		switch it.Action {
		case connio.ActionSkip:
			skips++
			_, _ = fmt.Fprintf(w, "- skip   %s: %s\n", esc(it.Label), esc(it.Reason))
		default:
			adds++
			c := it.Conn
			head := "+ add    " + esc(c.Name)
			if it.Action == connio.ActionRename {
				renames++
				head = "~ rename " + esc(it.From) + " -> " + esc(c.Name)
			}
			if it.Original != it.From {
				head += fmt.Sprintf(" (from %q)", esc(it.Original))
			}
			line := fmt.Sprintf("%s (%s) %s", head, esc(c.Driver), esc(driver.RedactDSN(c.DSN)))
			switch it.mode {
			case pwNone:
				if c.Password != "" {
					line += "; password: " + esc(c.Password)
				}
			case pwAsk:
				line += "; password: will ask to store it in the OS keyring"
			case pwStore:
				line += "; password: will store it in the OS keyring"
			case pwSkip:
				line += "; password: skipped (" + esc(it.why) + ")"
			}
			_, _ = fmt.Fprintln(w, line)
		}
		for _, n := range it.Notes {
			_, _ = fmt.Fprintf(w, "    note: %s\n", esc(n))
		}
	}
	_, _ = fmt.Fprintf(w, "%d to add (%d renamed), %d skipped\n", adds, renames, skips)
}

func (a *app) applyImport(cmd *cobra.Command, wc writeContext, items []importItem) error {
	errOut := a.streams.Err
	in := bufio.NewReader(a.streams.In)
	for i := range items {
		it := &items[i]
		if it.mode != pwAsk {
			continue
		}
		_, _ = fmt.Fprintf(errOut, "store the password found for %s in the OS keyring? [y/N] ", query.EscapeControls(it.Conn.Name))
		answer, _ := in.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
			it.mode = pwStore
		default:
			it.mode, it.why = pwSkip, "declined"
		}
	}
	// Check the config is still the version we read before storing anything.
	cur, err := config.StatOf(wc.path)
	if err != nil {
		return usageError(err)
	}
	if !cur.Equal(wc.stat) {
		return writeError(config.ErrConflict, "")
	}
	var stored []string
	for i := range items {
		it := &items[i]
		if it.mode != pwStore {
			continue
		}
		name := it.Conn.Name
		if err := a.deps.Keyring.Set(keyring.Service, name, it.Password.Reveal()); err != nil {
			_, _ = fmt.Fprintf(errOut, "warning: %s; the connection is added without a password\n",
				keyring.SetError(query.EscapeControls(name), err))
			continue
		}
		it.Conn.Password = keyring.Ref(name)
		stored = append(stored, name)
		_, _ = fmt.Fprintf(errOut, "stored password for %s in the OS keyring\n", query.EscapeControls(name))
	}
	var conns []config.Connection
	for _, it := range items {
		if it.Action != connio.ActionSkip {
			conns = append(conns, it.Conn)
		}
	}
	if _, err := config.AddConnections(wc.path, conns, wc.stat); err != nil {
		if len(stored) > 0 {
			_, _ = fmt.Fprintf(errOut, "warning: the passwords of %s were stored in the OS keyring but no connection was added\n",
				query.EscapeControls(strings.Join(stored, ", ")))
		}
		return writeError(err, "")
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "added %s\n", plural(int64(len(conns)), "connection", "connections"))
	return nil
}
