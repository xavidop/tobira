package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"slices"
	"strings"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/history"
	"github.com/xavidop/tobira/plugin/host"
)

// checkAI reports the ai: section offline: the provider, the model, the
// key (present, never its value) and the embedder config. With --online it
// adds the provider check the binary wires (Deps.CheckAI).
func (r *doctorRun) checkAI() []doctorCheck {
	row := func(name, status, detail string) doctorCheck {
		return doctorCheck{Section: "ai", Name: name, Status: status, Detail: detail}
	}
	if r.cfg == nil {
		return nil
	}
	if r.cfg.AI == nil {
		return []doctorCheck{row("ai", statusOK, "not configured (no ai: section); tobira never contacts a model")}
	}
	cfg := *r.cfg.AI
	var out []doctorCheck
	if r.a.deps.NewAssistant == nil {
		out = append(out, row("provider", statusWarn, cfg.Provider+", but this binary has no AI assistant"))
	} else {
		out = append(out, row("provider", statusOK, cfg.Provider))
	}
	model := cfg.Model
	if model == "" {
		model = "provider default"
	}
	out = append(out, row("model", statusOK, model))
	out = append(out, r.aiKey(cfg))
	if name := strings.TrimSpace(cfg.Embedder); name != "" {
		switch _, err := r.a.buildEmbedder(r.ctx, r.cfg, r.log, false); {
		case r.a.deps.NewEmbedder == nil:
			out = append(out, row("embedder", statusWarn, name+", but this binary has no embedder"))
		case err != nil:
			out = append(out, row("embedder", statusFail, errLine(err)))
		default:
			out = append(out, row("embedder", statusOK, name+" (config valid; not contacted)"))
		}
		if c, ok := r.embedderKey(cfg, name); ok {
			out = append(out, c)
		}
	}
	if r.f.online {
		out = append(out, r.aiOnline(cfg))
	}
	return out
}

// aiKey reports where the provider's key comes from: the ai.api_key ref
// (its refs result), else the first provider variable that is set.
func (r *doctorRun) aiKey(cfg config.AI) doctorCheck {
	c := doctorCheck{Section: "ai", Name: "api_key"}
	vars := config.AIKeyEnv(cfg.Provider)
	for _, v := range vars {
		// The online check may read these: mask them whichever key is used.
		r.sawSecret(driver.NewSecret(r.a.getenv(v)))
	}
	switch {
	case len(vars) == 0:
		c.Status, c.Detail = statusOK, "no key needed for "+cfg.Provider
		return c
	case cfg.APIKey != "":
		ref := r.refs["ai.api_key"]
		c.Status, c.Detail = ref.Status, "ai.api_key: "+ref.Detail
		return c
	}
	for _, v := range vars {
		if r.a.getenv(v) != "" {
			c.Status, c.Detail = statusOK, "from "+v
			return c
		}
	}
	if cfg.Provider == "openai" && cfg.BaseURL != "" {
		c.Status, c.Detail = statusOK, "no key, for an OpenAI-compatible base_url"
		return c
	}
	c.Status, c.Detail = statusFail, fmt.Sprintf("no key: set ai.api_key (a secret ref) or %s", vars[0])
	return c
}

// embedderKey checks the key of an embedder on another provider than
// ai.provider ("googleai/text-embedding-005" with provider: anthropic),
// which takes its key from that provider's variable only (ai.api_key
// applies to ai.provider). ok is false when there is no such provider or
// it is ai.provider, whose key the api_key row covers.
func (r *doctorRun) embedderKey(cfg config.AI, name string) (doctorCheck, bool) {
	i := strings.IndexByte(name, '/')
	if i <= 0 || !slices.Contains(config.AIProviders(), name[:i]) || name[:i] == cfg.Provider {
		return doctorCheck{}, false
	}
	prov := name[:i]
	c := doctorCheck{Section: "ai", Name: "embedder api_key"}
	vars := config.AIKeyEnv(prov)
	if len(vars) == 0 {
		c.Status, c.Detail = statusOK, "no key needed for "+prov
		return c, true
	}
	for _, v := range vars {
		r.sawSecret(driver.NewSecret(r.a.getenv(v)))
	}
	for _, v := range vars {
		if r.a.getenv(v) != "" {
			c.Status, c.Detail = statusOK, prov+" key from "+v
			return c, true
		}
	}
	c.Status, c.Detail = statusFail, fmt.Sprintf("no key for the embedder's provider %s: set %s", prov, vars[0])
	return c, true
}

// aiOnline asks the provider over the network whether it answers.
func (r *doctorRun) aiOnline(cfg config.AI) doctorCheck {
	c := doctorCheck{Section: "ai", Name: "online"}
	if r.a.deps.CheckAI == nil {
		c.Status, c.Detail = statusWarn, "this binary has no online AI check"
		return c
	}
	ctx, cancel := context.WithTimeout(r.ctx, doctorAITimeout)
	defer cancel()
	_, err := within(ctx, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, r.a.deps.CheckAI(ctx, cfg, AICheckDeps{Secrets: r.res, Getenv: r.a.deps.Getenv})
	})
	switch {
	case err == nil:
	case r.ctx.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		c.Status, c.Detail = statusFail, fmt.Sprintf("%s did not answer within %s", cfg.Provider, doctorAITimeout)
		return c
	default:
		c.Status, c.Detail = statusFail, errLine(err)
		return c
	}
	c.Status, c.Detail = statusOK, cfg.Provider+" answered"
	return c
}

// checkMCP reports the mcp: section: every exposed name must be a loaded
// connection, allow_writes a subset of expose, and mcp.token (when set)
// must resolve.
func (r *doctorRun) checkMCP() []doctorCheck {
	row := func(name, status, detail string) doctorCheck {
		return doctorCheck{Section: "mcp", Name: name, Status: status, Detail: detail}
	}
	if r.cfg == nil {
		return nil
	}
	if r.cfg.MCP == nil {
		return []doctorCheck{row("mcp", statusOK, "not configured; nothing is exposed")}
	}
	m := r.cfg.MCPSettings()
	var out []doctorCheck
	if len(m.Expose) == 0 {
		out = append(out, row("expose", statusOK, "mcp.expose is empty; tobira mcp needs --expose"))
	}
	loaded := func(name string) bool {
		return slices.ContainsFunc(r.cfg.Connections, func(c config.Connection) bool { return c.Name == name })
	}
	for _, name := range m.Expose {
		switch {
		case loaded(name):
			out = append(out, row("expose "+name, statusOK, "connection exists"))
		case slices.ContainsFunc(r.cfg.Broken, func(b config.BrokenConnection) bool { return b.Name == name }):
			out = append(out, row("expose "+name, statusFail, "that connection is invalid and will not be served"))
		default:
			out = append(out, row("expose "+name, statusFail, "no connection with that name"))
		}
	}
	for _, name := range m.AllowWrites {
		if slices.Contains(m.Expose, name) {
			out = append(out, row("allow_writes "+name, statusOK, "writes allowed over MCP"))
		} else {
			out = append(out, row("allow_writes "+name, statusWarn, "not in mcp.expose, so it is not writable"))
		}
	}
	if m.Token == "" {
		out = append(out, row("token", statusOK, "not set; tobira mcp --http generates one at start"))
	} else {
		ref := r.refs["mcp.token"]
		out = append(out, row("token", ref.Status, "mcp.token: "+ref.Detail+" (used by --http)"))
	}
	return out
}

// checkHistory inspects the history file at history.path (else the default
// location), if it exists, without changing it: it opens it read-only and
// reports its schema version and size, and checks with an access check
// that tobira could write to it. It creates, chmods and migrates nothing.
func (r *doctorRun) checkHistory() []doctorCheck {
	row := func(status, detail string) []doctorCheck {
		return []doctorCheck{{Section: "history", Name: "db", Status: status, Detail: detail}}
	}
	if r.cfg == nil {
		return row(statusWarn, "the config did not load, so history.path is unknown")
	}
	path, err := r.a.historyPath(r.cfg)
	switch {
	case err != nil && r.cfg.HistoryPath() != "":
		return row(statusFail, errLine(err))
	case err != nil:
		return row(statusWarn, "cannot locate the history file; set XDG_STATE_HOME or history.path")
	}
	where := path
	if r.cfg.HistoryPath() != "" {
		where += " (history.path)"
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return row(statusOK, "not created yet ("+where+"); the first query creates it")
	}
	if _, ok := r.a.deps.Registry.Lookup("sqlite"); !ok {
		return row(statusWarn, "this binary has no sqlite driver, so history is off")
	}
	s, err := history.OpenReadOnly(r.ctx, r.a.deps.Registry, path)
	if err != nil {
		return row(statusFail, errLine(err))
	}
	schema := fmt.Sprintf("schema v%d", s.SchemaVersion())
	if s.SchemaVersion() == history.SchemaNone {
		schema = "no history table yet"
	}
	_ = s.Close()
	size := "size unknown"
	if st, err := os.Stat(path); err == nil {
		size = humanSize(st.Size())
	}
	if err := history.Writable(path); err != nil {
		return row(statusWarn, fmt.Sprintf("%s, %s, but %s, so queries are not recorded: %s", schema, size, errLine(err), where))
	}
	return row(statusOK, fmt.Sprintf("%s, %s, writable, %s", schema, size, where))
}

// humanSize formats n bytes as B, KiB, MiB or GiB.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 2; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMG"[exp])
}

// checkPermissions reports whether the config file and the history file
// are group- or world-accessible (a warning) and whether a plugins
// directory others can write to (world-writable fails: anyone could add a
// plugin; group-writable or another user's warns).
func (r *doctorRun) checkPermissions() []doctorCheck {
	row := func(name, status, detail string) doctorCheck {
		return doctorCheck{Section: "permissions", Name: name, Status: status, Detail: detail}
	}
	if runtime.GOOS == "windows" {
		return []doctorCheck{row("permissions", statusOK, "not checked on Windows")}
	}
	var out []doctorCheck
	if path := r.a.configPath(r.cmd); path != "" {
		s, d := filePerm(path)
		out = append(out, row("config", s, d))
	}
	if r.cfg != nil {
		if path, err := r.a.historyPath(r.cfg); err == nil {
			s, d := filePerm(path)
			out = append(out, row("history", s, d))
		}
	}
	for _, dir := range r.a.pluginDirs(r.a.peekConfig(r.cmd).PluginSettings()) {
		s, d := dirPerm(dir)
		out = append(out, row("plugins dir", s, d))
	}
	return out
}

// filePerm is a private file's status: ok for no group or world bits.
func filePerm(path string) (status, detail string) {
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return statusOK, path + " does not exist"
	}
	if err != nil {
		return statusWarn, path + " cannot be checked: " + errText(err)
	}
	perm := st.Mode().Perm()
	var bad []string
	for _, b := range []struct {
		bit  fs.FileMode
		what string
	}{{0o040, "group-readable"}, {0o020, "group-writable"}, {0o004, "world-readable"}, {0o002, "world-writable"}} {
		if perm&b.bit != 0 {
			bad = append(bad, b.what)
		}
	}
	if len(bad) == 0 {
		return statusOK, fmt.Sprintf("%04o %s", perm, path)
	}
	return statusWarn, fmt.Sprintf("%04o is %s; run chmod 600 %s", perm, strings.Join(bad, ", "), path)
}

// dirPerm is a plugins directory's status.
func dirPerm(dir string) (status, detail string) {
	st, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return statusOK, dir + " does not exist"
	}
	if err != nil {
		return statusWarn, dir + " cannot be checked: " + errText(err)
	}
	perm := st.Mode().Perm()
	switch {
	case perm&0o002 != 0:
		return statusFail, fmt.Sprintf("%04o is world-writable: anyone can add a plugin; run chmod 700 %s", perm, dir)
	case perm&0o020 != 0:
		return statusWarn, fmt.Sprintf("%04o is group-writable; run chmod 700 %s", perm, dir)
	}
	if why := host.UnsafeDir(dir); why != "" {
		return statusWarn, dir + " is " + why
	}
	return statusOK, fmt.Sprintf("%04o %s", perm, dir)
}

// errText is a short reason for err without the path it may repeat.
func errText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}
