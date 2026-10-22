package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/secrets/keyring"
)

var errNoKeyring = errors.New("the OS keyring is not available in this build")

func (a *app) newConnSetPasswordCmd() *cobra.Command {
	var noConfig, stdin, force bool
	cmd := &cobra.Command{
		Use:   "set-password <name>",
		Short: "Store a connection's password in the OS keyring",
		Long: "Store the password of a connection in the OS keyring (service \"tobira\", account\n" +
			"<name>) and point the connection's password: at keyring:<name>.\n\n" +
			"The password is read without echo from the terminal and asked twice. It is never\n" +
			"accepted as an argument. Without a terminal, pass --stdin to read one line from\n" +
			"standard input. With --no-config the config file is left untouched, so <name>\n" +
			"need not be a configured connection.\n\n" +
			"When another connection's password: is keyring:<name> (one renamed from <name>\n" +
			"keeps it), storing would change its password too, so set-password refuses\n" +
			"unless --force is given.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runSetPassword(cmd, args[0], noConfig, stdin, force)
		},
	}
	cmd.Flags().BoolVar(&noConfig, "no-config", false, "only store the password; do not change the config file")
	cmd.Flags().BoolVar(&stdin, "stdin", false, "read the password from one line of standard input (no prompt)")
	cmd.Flags().BoolVar(&force, "force", false, "store even when another connection uses the same keyring entry")
	return cmd
}

func (a *app) runSetPassword(cmd *cobra.Command, name string, noConfig, stdin, force bool) error {
	if err := keyring.ValidName(name); err != nil {
		return usageError(err)
	}
	if a.deps.Keyring == nil {
		return usageError(errNoKeyring)
	}
	if stdin && a.deps.IsInputTTY != nil && a.deps.IsInputTTY() {
		return usageError(errors.New("--stdin needs piped input; on a terminal, run without it to be prompted"))
	}
	var (
		wc writeContext
		t  target
	)
	if !noConfig {
		var err error
		if wc, err = a.writeCtx(cmd); err != nil {
			return err
		}
		if t, err = findTarget(wc.cfg, cmd, []string{name}); err != nil {
			return err
		}
	}
	if !force {
		cfg := wc.cfg
		if noConfig {
			cfg = a.peekConfig(cmd)
		}
		if other, ok := keyring.SharedWith(cfg, name); ok {
			return usageError(fmt.Errorf("%s is used by connection %s; pass --force to overwrite",
				keyring.Ref(name), query.EscapeControls(other)))
		}
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	secret, err := a.readNewPassword(ctx, name, stdin)
	if err != nil {
		return err
	}
	// Check the config is still the version we read before storing anything,
	// so a change made while the user typed stores nothing.
	if !noConfig {
		cur, err := config.StatOf(wc.path)
		if err != nil || !cur.Equal(wc.stat) {
			return writeError(config.ErrConflict, name)
		}
	}
	if err := a.deps.Keyring.Set(keyring.Service, name, secret); err != nil {
		return usageError(keyring.SetError(query.EscapeControls(name), err))
	}
	if !noConfig {
		if err := a.pointToKeyring(wc, t, name); err != nil {
			return usageError(fmt.Errorf("the password was stored in the OS keyring but the config was not updated (%s); "+
				"point the password key of %s at %s by hand", err, query.EscapeControls(name), keyring.Ref(name)))
		}
	}
	_, _ = fmt.Fprintf(a.streams.Err, "stored password for %s in the OS keyring\n", query.EscapeControls(name))
	return nil
}

// peekConfig loads the config only to look at it, without the broken-entry
// warnings. It returns nil when there is none or it does not load.
func (a *app) peekConfig(cmd *cobra.Command) *config.Config {
	path := a.configPath(cmd)
	if path == "" {
		return nil
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil
	}
	return cfg
}

// pointToKeyring sets the entry's password: to keyring:<name>, changing
// nothing else in the entry.
func (a *app) pointToKeyring(wc writeContext, t target, name string) error {
	ref := keyring.Ref(name)
	old := t.conn.Password
	if old == ref {
		return nil
	}
	if _, err := config.SetPasswordRefAt(wc.path, t.line, ref, wc.stat); err != nil {
		return errors.New(query.EscapeControls(driver.RedactMessage(err.Error(), driver.ConnConfig{})))
	}
	if old != "" {
		shown := "(not a secret reference)"
		if config.ValidateLoadedPasswordRef(old) == nil {
			shown = query.EscapeControls(driver.RedactMessage(old, driver.ConnConfig{}))
		}
		_, _ = fmt.Fprintf(a.streams.Err, "password ref changed from %s to %s\n", shown, ref)
	}
	return nil
}

// readNewPassword returns the password to store. On a terminal it prompts
// twice with no echo; with stdin it reads one line.
func (a *app) readNewPassword(ctx context.Context, name string, stdin bool) (string, error) {
	errOut := a.streams.Err
	if stdin {
		line, err := bufio.NewReader(a.streams.In).ReadString('\n')
		if err != nil && (!errors.Is(err, io.EOF) || line == "") {
			return "", usageError(errors.New("no password on standard input"))
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			return "", usageError(errors.New("the password is empty; nothing stored"))
		}
		return line, nil
	}
	if a.deps.IsInputTTY == nil || !a.deps.IsInputTTY() || a.deps.ReadPassword == nil {
		return "", usageError(errors.New("set-password needs an interactive terminal; use --stdin to read the password from standard input"))
	}
	ask := func(prompt string) (string, error) {
		_, _ = fmt.Fprint(errOut, prompt)
		pw, err := a.deps.ReadPassword(ctx)
		_, _ = fmt.Fprintln(errOut)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr // cancelled: the usual cancel exit
		}
		if err != nil {
			return "", usageError(errors.New("could not read the password"))
		}
		return pw, nil
	}
	first, err := ask("Password for " + query.EscapeControls(name) + ": ")
	if err != nil {
		return "", err
	}
	second, err := ask("Confirm: ")
	if err != nil {
		return "", err
	}
	switch {
	case first == "":
		return "", usageError(errors.New("the password is empty; nothing stored"))
	case first != second:
		return "", usageError(errors.New("the passwords do not match; nothing stored"))
	}
	return first, nil
}
