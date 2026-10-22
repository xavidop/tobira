package cli

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/xavidop/tobira/internal/session"
	"github.com/xavidop/tobira/plugin/host"
)

// Bounds on resolving driver plugins' Info before the AI tools are built.
const (
	// resolveParallel is how many plugins are asked at once.
	resolveParallel = 4
	// resolveTimeout bounds each plugin's start and Info.
	resolveTimeout = 5 * time.Second
)

// resolvePlugins makes sure every driver plugin the named connections use
// has reported its real Info, so the Manager's State (and with it the AI
// tools' read-only gate, the similar tool's registration and
// list_connections) sees the plugin's capabilities and kind rather than
// the stub. Plugins that already answered are not asked again; the others
// are started, at most resolveParallel at a time, each bounded by
// resolveTimeout. It blocks; call it with no lock held, never from a TUI
// Update.
//
// It returns the connections whose plugin could not answer, each mapped to
// a redacted one-line reason naming the plugin. Connections on built-in
// drivers, broken ones and unknown names are left out. When ctx itself ends
// (the caller cancelled, or its deadline passed) it returns ctx.Err()
// instead: that is the caller's interrupt, not the plugin's failure. A
// plugin's own resolveTimeout, with ctx alive, is the plugin's failure.
func (a *app) resolvePlugins(ctx context.Context, mgr *session.Manager, names []string, log *slog.Logger) (map[string]string, error) {
	if a.plugins == nil || mgr == nil {
		return nil, nil
	}
	// The connections of each plugin not yet started.
	byPlugin := map[string][]string{}
	var order []string
	for _, name := range names {
		st := mgr.State(name)
		if st.Broken || st.Info.Name == "" {
			continue
		}
		d, ok := a.deps.Registry.Lookup(st.Info.Name)
		if !ok {
			continue
		}
		if _, isPlugin := host.PluginOf(d); !isPlugin {
			continue
		}
		if _, started := host.InfoIfStarted(d); started {
			continue
		}
		if _, seen := byPlugin[st.Info.Name]; !seen {
			order = append(order, st.Info.Name)
		}
		byPlugin[st.Info.Name] = append(byPlugin[st.Info.Name], name)
	}
	if len(order) == 0 {
		return nil, nil
	}
	errs := make([]error, len(order))
	sem := make(chan struct{}, resolveParallel)
	var wg sync.WaitGroup
	for i, plugin := range order {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, resolveTimeout)
			defer cancel()
			_, errs[i] = a.plugins.Info(pctx, plugin)
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out map[string]string
	for i, plugin := range order {
		if errs[i] == nil {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		for _, conn := range byPlugin[plugin] {
			why := fmt.Sprintf("driver plugin %q did not answer: %s", plugin, firstLine(mgr.Redact(conn, errs[i].Error())))
			log.Debug("plugin unavailable", "plugin", plugin, "connection", conn, "error", why)
			out[conn] = why
		}
	}
	return out, nil
}

// pluginResolver is resolvePlugins for one connection, as the TUI's AI
// panel calls it from a turn's goroutine: nil when the connection's driver
// plugin answered (or it uses none), ctx.Err() when ctx ended first, else
// an error with the reason.
func (a *app) pluginResolver(mgr *session.Manager, log *slog.Logger) func(context.Context, string) error {
	if a.plugins == nil {
		return nil
	}
	return func(ctx context.Context, conn string) error {
		un, err := a.resolvePlugins(ctx, mgr, []string{conn}, log)
		if err != nil {
			return err
		}
		if why, ok := un[conn]; ok {
			return errUnavailableConn(conn, why)
		}
		return nil
	}
}

// errUnavailableConn says a connection cannot be used because its driver
// plugin did not answer.
func errUnavailableConn(conn, why string) error {
	return fmt.Errorf("connection %q is unavailable: %s", conn, why)
}

// firstLine is s up to its first line break, trimmed.
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
