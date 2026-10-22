package cli_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/cli"
)

// driverEnv is a tobira home with a plugins dir and a PATH dir, and a
// registry holding the built-in fake driver.
type driverEnv struct {
	t       *testing.T
	xdg     string
	plugins string // $XDG_CONFIG_HOME/tobira/plugins
	pathDir string
	config  string
	tty     bool
}

func newDriverEnv(t *testing.T) *driverEnv {
	t.Helper()
	xdg := t.TempDir()
	e := &driverEnv{
		t: t, xdg: xdg, plugins: filepath.Join(xdg, "tobira", "plugins"),
		pathDir: t.TempDir(), config: filepath.Join(t.TempDir(), "config.yaml"),
	}
	if err := os.WriteFile(e.config, []byte("connections: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return e
}

// run executes tobira with args and returns stdout, stderr and the exit code.
func (e *driverEnv) run(args ...string) (string, string, int) {
	e.t.Helper()
	reg := driver.NewRegistry()
	if err := reg.Add(drivertest.NewFake().Driver()); err != nil {
		e.t.Fatal(err)
	}
	env := map[string]string{"XDG_CONFIG_HOME": e.xdg, "PATH": e.pathDir}
	var out, errOut bytes.Buffer
	root := cli.NewRoot(cli.BuildInfo{Version: "test"}, cli.Streams{Out: &out, Err: &errOut}, cli.Deps{
		Registry: reg,
		Getenv:   func(k string) string { return env[k] },
		Home:     e.t.TempDir(),
		Environ:  func() []string { return nil },
		IsTTY:    func(io.Writer) bool { return e.tty },
	})
	code := cli.Execute(context.Background(), root, append([]string{"--config", e.config}, args...))
	return out.String(), errOut.String(), code
}

type listRow struct {
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	Version      string   `json:"version"`
	Source       string   `json:"source"`
	Status       string   `json:"status"`
	Capabilities []string `json:"capabilities"`
}

func decodeList(t *testing.T, out string) []listRow {
	t.Helper()
	var rows []listRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("driver list output is not a JSON array: %v\n%s", err, out)
	}
	return rows
}

func rowsNamed(rows []listRow, name string) []listRow {
	var out []listRow
	for _, r := range rows {
		if r.Name == name {
			out = append(out, r)
		}
	}
	return out
}

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("exec bits are unix semantics")
	}
}

func TestDriverListNoStart(t *testing.T) {
	skipOnWindows(t)
	e := newDriverEnv(t)
	fakePlugin(t, e.plugins, "ext")
	fakePlugin(t, e.pathDir, "ext")  // shadowed by the plugins dir
	fakePlugin(t, e.pathDir, "fake") // shadowed by the built-in
	out, errOut, code := e.run("driver", "list", "--no-start")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	rows := decodeList(t, out)

	fake := rowsNamed(rows, "fake")
	if len(fake) != 2 || fake[0].Source != "builtin" || fake[0].Status != "ok" || len(fake[0].Capabilities) == 0 {
		t.Fatalf("fake rows = %+v, want the built-in first, with capabilities", fake)
	}
	if want := "plugin:" + filepath.Join(e.pathDir, "tobira-driver-fake"); !strings.HasPrefix(fake[1].Status, "shadowed:") || fake[1].Source != resolved(t, want) {
		t.Errorf("shadowed fake plugin row = %+v", fake[1])
	}

	ext := rowsNamed(rows, "ext")
	if len(ext) != 2 {
		t.Fatalf("ext rows = %+v, want the plugin and its shadowed PATH copy", ext)
	}
	if ext[0].Source != resolved(t, "plugin:"+filepath.Join(e.plugins, "tobira-driver-ext")) || ext[0].Status != "not started" ||
		ext[0].Kind != "other" || len(ext[0].Capabilities) != 0 {
		t.Errorf("ext row = %+v, want an unstarted plugin from the plugins dir", ext[0])
	}
	if ext[1].Source != resolved(t, "plugin:"+filepath.Join(e.pathDir, "tobira-driver-ext")) || !strings.HasPrefix(ext[1].Status, "shadowed:") {
		t.Errorf("shadowed ext row = %+v", ext[1])
	}
	for _, want := range []string{`a built-in driver named "fake" wins`, "shadowed by " + resolved(t, filepath.Join(e.plugins, "tobira-driver-ext"))} {
		if !strings.Contains(errOut, "warning: ") || !strings.Contains(errOut, want) {
			t.Errorf("stderr %q lacks a warning with %q", errOut, want)
		}
	}
}

// resolved resolves symlinks in a "plugin:<path>" source or a path (macOS
// temp dirs live under a symlinked /var).
func resolved(t *testing.T, s string) string {
	t.Helper()
	prefix := ""
	if rest, ok := strings.CutPrefix(s, "plugin:"); ok {
		prefix, s = "plugin:", rest
	}
	r, err := filepath.EvalSymlinks(s)
	if err != nil {
		t.Fatal(err)
	}
	return prefix + r
}

func TestDriverListStartsPlugins(t *testing.T) {
	skipOnWindows(t)
	e := newDriverEnv(t)
	fakePlugin(t, e.plugins, "broken") // exits at once: the handshake fails
	out, errOut, code := e.run("driver", "list")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	broken := rowsNamed(decodeList(t, out), "broken")
	if len(broken) != 1 || !strings.HasPrefix(broken[0].Status, "error: ") {
		t.Fatalf("broken rows = %+v, want status error: ...", broken)
	}
}

func TestDriverListTable(t *testing.T) {
	skipOnWindows(t)
	e := newDriverEnv(t)
	e.tty = true
	fakePlugin(t, e.plugins, "ext")
	out, errOut, code := e.run("driver", "list", "--no-start")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, col := range []string{"NAME", "KIND", "VERSION", "SOURCE", "STATUS", "CAPABILITIES"} {
		if !strings.Contains(lines[0], col) {
			t.Errorf("header %q lacks %s", lines[0], col)
		}
	}
	if !strings.Contains(out, "builtin") || !strings.Contains(out, "not started") || !strings.Contains(out, "browser") {
		t.Errorf("table:\n%s", out)
	}
}

func TestDriverListPluginsDisabled(t *testing.T) {
	skipOnWindows(t)
	e := newDriverEnv(t)
	fakePlugin(t, e.plugins, "ext")
	if err := os.WriteFile(e.config, []byte("plugins:\n  enabled: false\nconnections: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := e.run("driver", "list")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	if rows := decodeList(t, out); len(rowsNamed(rows, "ext")) != 0 || len(rowsNamed(rows, "fake")) != 1 {
		t.Errorf("rows = %+v, want only the built-in", rows)
	}
	if !strings.Contains(errOut, "plugins are disabled") {
		t.Errorf("stderr %q does not say plugins are disabled", errOut)
	}
}

type infoOut struct {
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	Source       string   `json:"source"`
	Status       string   `json:"status"`
	Schemes      []string `json:"schemes"`
	Dialect      string   `json:"dialect"`
	Path         string   `json:"path"`
	SHA256       string   `json:"sha256"`
	Capabilities []struct {
		Name      string `json:"name"`
		Supported bool   `json:"supported"`
	} `json:"capabilities"`
	Conflicts []string `json:"conflicts"`
}

func decodeInfo(t *testing.T, out string) infoOut {
	t.Helper()
	var info infoOut
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("driver info output is not JSON: %v\n%s", err, out)
	}
	return info
}

func TestDriverInfoBuiltin(t *testing.T) {
	skipOnWindows(t)
	e := newDriverEnv(t)
	fakePlugin(t, e.pathDir, "fake") // shadowed by the built-in
	out, errOut, code := e.run("driver", "info", "fake")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	info := decodeInfo(t, out)
	want := drivertest.NewFake().Driver().Info()
	if info.Name != "fake" || info.Source != "builtin" || info.Status != "ok" || info.Dialect != want.Dialect ||
		len(info.Schemes) != len(want.Schemes) || info.Path != "" || info.SHA256 != "" {
		t.Errorf("info = %+v, want the fake driver's Info", info)
	}
	supported := map[string]bool{}
	for _, c := range info.Capabilities {
		supported[c.Name] = c.Supported
	}
	if len(info.Capabilities) != 11 {
		t.Errorf("capability matrix has %d entries, want all 11", len(info.Capabilities))
	}
	for _, c := range want.Capabilities {
		if !supported[string(c)] {
			t.Errorf("matrix says %s is unsupported", c)
		}
	}
	if len(info.Conflicts) != 1 || !strings.Contains(info.Conflicts[0], "tobira-driver-fake") {
		t.Errorf("conflicts = %q, want the shadowed plugin", info.Conflicts)
	}

	e.tty = true
	out, _, code = e.run("driver", "info", "fake")
	if code != 0 || !strings.Contains(out, "browser") || !strings.Contains(out, "builtin") {
		t.Errorf("table info (code %d):\n%s", code, out)
	}
}

func TestDriverInfoPlugin(t *testing.T) {
	skipOnWindows(t)
	e := newDriverEnv(t)
	fakePlugin(t, e.plugins, "broken")
	fakePlugin(t, e.pathDir, "broken")
	out, errOut, code := e.run("driver", "info", "broken")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	info := decodeInfo(t, out)
	path := resolved(t, filepath.Join(e.plugins, "tobira-driver-broken"))
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if info.Path != path || info.Source != "plugin:"+path || info.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("info = %+v, want path %s and its sha256", info, path)
	}
	if !strings.HasPrefix(info.Status, "error: ") {
		t.Errorf("status = %q, want error: ...", info.Status)
	}
	if len(info.Conflicts) != 1 || !strings.Contains(info.Conflicts[0], e.pathDir) && !strings.Contains(info.Conflicts[0], resolved(t, e.pathDir)) {
		t.Errorf("conflicts = %q, want the shadowed PATH copy", info.Conflicts)
	}
}

func TestDriverInfoUnknown(t *testing.T) {
	e := newDriverEnv(t)
	_, errOut, code := e.run("driver", "info", "nope")
	if code != 1 || !strings.Contains(errOut, `unknown driver "nope"`) {
		t.Errorf("code %d, stderr %q; want exit 1 and unknown driver", code, errOut)
	}
}

func sum256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// installPayload is a plugin that would leave a marker file if it ran.
func installPayload(marker string) []byte {
	return []byte("#!/bin/sh\ntouch " + marker + "\n")
}

func serveBytes(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	t.Cleanup(srv.Close)
	return srv
}

func TestDriverInstall(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	body := installPayload(marker)
	srv := serveBytes(t, body)
	url := srv.URL + "/tobira-driver-demo"

	t.Run("happy path", func(t *testing.T) {
		e := newDriverEnv(t)
		out, errOut, code := e.run("driver", "install", url, "--sha256", sum256(body))
		if code != 0 {
			t.Fatalf("code %d, stderr %q", code, errOut)
		}
		dest := filepath.Join(e.plugins, "tobira-driver-demo")
		if runtime.GOOS == "windows" {
			dest += ".exe"
		}
		if got, err := os.ReadFile(dest); err != nil || !bytes.Equal(got, body) {
			t.Fatalf("installed %q, %v", got, err)
		}
		for _, want := range []string{`"demo"`, dest, "tobira driver info demo"} {
			if !strings.Contains(out, want) {
				t.Errorf("stdout %q lacks %q", out, want)
			}
		}
		if _, err := os.Stat(marker); err == nil {
			t.Error("install ran the plugin")
		}
		// The installed plugin is discovered by the next command.
		out, _, _ = e.run("driver", "list", "--no-start")
		if len(rowsNamed(decodeList(t, out), "demo")) != 1 {
			t.Errorf("driver list does not show the installed plugin:\n%s", out)
		}
		// Again: refused without --force, replaced with it.
		if _, errOut, code := e.run("driver", "install", url, "--sha256", sum256(body)); code != 1 || !strings.Contains(errOut, "--force") {
			t.Errorf("reinstall: code %d, stderr %q; want exit 1 naming --force", code, errOut)
		}
		if _, errOut, code := e.run("driver", "install", url, "--sha256", sum256(body), "--force"); code != 0 {
			t.Errorf("reinstall --force: code %d, stderr %q", code, errOut)
		}
	})

	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"missing sha256", []string{url}, 1, "--sha256 is required"},
		{"malformed sha256", []string{url, "--sha256", "abc"}, 1, "64 hex characters"},
		{"non-hex sha256", []string{url, "--sha256", strings.Repeat("g", 64)}, 1, "64 hex characters"},
		{"bad checksum", []string{url, "--sha256", sum256(nil)}, 3, "checksum"},
		{"traversal", []string{url, "--sha256", sum256(body), "--name", "../x"}, 1, "plain file name"},
		{"separator", []string{url, "--sha256", sum256(body), "--name", "a/b"}, 1, "plain file name"},
		{"prefixed traversal", []string{url, "--sha256", sum256(body), "--name", "tobira-driver-../.."}, 1, "plain file name"},
		{"invalid name", []string{url, "--sha256", sum256(body), "--name", "Bad_Name"}, 1, "must match"},
		{"file URL", []string{"file:///etc/passwd", "--sha256", sum256(body)}, 1, "http and https"},
		{"negative timeout", []string{url, "--sha256", sum256(body), "--timeout", "-1s"}, 1, "--timeout"},
		{"no source", []string{"--sha256", sum256(body)}, 1, "argument"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newDriverEnv(t)
			_, errOut, code := e.run(append([]string{"driver", "install"}, c.args...)...)
			if code != c.code || !strings.Contains(errOut, c.want) {
				t.Errorf("code %d, stderr %q; want exit %d with %q", code, errOut, c.code, c.want)
			}
			if es, _ := os.ReadDir(e.plugins); len(es) != 0 {
				t.Errorf("plugins dir holds %d entries", len(es))
			}
		})
	}

	t.Run("over the cap", func(t *testing.T) {
		big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "268435457") // 256 MiB + 1
		}))
		t.Cleanup(big.Close)
		e := newDriverEnv(t)
		_, errOut, code := e.run("driver", "install", big.URL+"/tobira-driver-demo", "--sha256", sum256(body))
		if code != 3 || !strings.Contains(errOut, "256 MiB") {
			t.Errorf("code %d, stderr %q; want exit 3 naming the 256 MiB cap", code, errOut)
		}
	})

	t.Run("redirect to file", func(t *testing.T) {
		redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
		}))
		t.Cleanup(redir.Close)
		e := newDriverEnv(t)
		_, errOut, code := e.run("driver", "install", redir.URL+"/tobira-driver-demo", "--sha256", sum256(body))
		if code != 2 || !strings.Contains(errOut, "redirect") {
			t.Errorf("code %d, stderr %q; want exit 2 and a refused redirect", code, errOut)
		}
	})

	t.Run("local path", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "tobira-driver-local")
		if err := os.WriteFile(src, body, 0o755); err != nil {
			t.Fatal(err)
		}
		e := newDriverEnv(t)
		out, errOut, code := e.run("driver", "install", src, "--sha256", sum256(body))
		if code != 0 || !strings.Contains(out, `"local"`) {
			t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Error("install ran the plugin")
		}
	})

	t.Run("directory source", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "tobira-driver-dir")
		if err := os.Mkdir(src, 0o755); err != nil {
			t.Fatal(err)
		}
		e := newDriverEnv(t)
		_, errOut, code := e.run("driver", "install", src, "--sha256", sum256(body))
		if code != 1 || !strings.Contains(errOut, "not a regular file") {
			t.Errorf("code %d, stderr %q; want exit 1, not a regular file", code, errOut)
		}
	})

	t.Run("world-writable plugins dir", func(t *testing.T) {
		skipOnWindows(t)
		e := newDriverEnv(t)
		if err := os.MkdirAll(e.plugins, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(e.plugins, 0o777); err != nil {
			t.Fatal(err)
		}
		_, errOut, code := e.run("driver", "install", url, "--sha256", sum256(body))
		if code != 4 || !strings.Contains(errOut, "world-writable") {
			t.Errorf("code %d, stderr %q; want exit 4, world-writable", code, errOut)
		}
	})

	t.Run("configured plugins dir", func(t *testing.T) {
		e := newDriverEnv(t)
		dir := filepath.Join(t.TempDir(), "mine")
		if err := os.WriteFile(e.config, []byte("plugins:\n  dirs:\n    - "+dir+"\nconnections: []\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, errOut, code := e.run("driver", "install", url, "--sha256", sum256(body)); code != 0 {
			t.Fatalf("code %d, stderr %q", code, errOut)
		}
		if _, err := os.Stat(filepath.Join(dir, filepath.Base(url))); err != nil && runtime.GOOS != "windows" {
			t.Errorf("not installed into the first plugins.dirs entry: %v", err)
		}
	})
}
