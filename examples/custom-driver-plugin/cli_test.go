package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/examples/custom-driver-plugin/memdemo"
)

// buildTobira builds the CLI module into a temp dir.
func buildTobira(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "tobira")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	goBuild(t, filepath.Join("..", "..", "cmd", "tobira"), bin)
	return bin
}

// TestQueryThroughTobira is the end-to-end path a user takes: build the
// plugin, put it in a plugins directory, name that directory in the
// config, and query a memdemo connection with the real tobira binary.
func TestQueryThroughTobira(t *testing.T) {
	pluginDir := buildPlugin(t)
	tobira := buildTobira(t)
	home := t.TempDir()
	cfg := filepath.Join(home, "config.yaml")
	config := "plugins:\n  dirs:\n    - " + pluginDir + "\n" +
		"connections:\n" +
		"  - name: demo\n    dsn: memdemo://\n" +
		"  - name: demo-ro\n    dsn: memdemo://\n    read_only: true\n"
	if err := os.WriteFile(cfg, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	// A PATH dir with plugins that lose: a memdemo the plugins dir shadows,
	// and a sqlite the built-in driver wins over. Neither is ever started.
	shadowDir := t.TempDir()
	if runtime.GOOS != "windows" {
		for _, name := range []string{"tobira-driver-memdemo", "tobira-driver-sqlite"} {
			if err := os.WriteFile(filepath.Join(shadowDir, name), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	env := []string{
		"PATH=" + shadowDir + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + home,
		"XDG_CONFIG_HOME=" + home, "XDG_STATE_HOME=" + home, "XDG_DATA_HOME=" + home,
	}
	tobiraRun := func(args ...string) (stdout, stderr string, err error) {
		var out, errOut bytes.Buffer
		cmd := exec.Command(tobira, append([]string{"--config", cfg}, args...)...)
		cmd.Env, cmd.Stdout, cmd.Stderr = env, &out, &errOut
		err = cmd.Run()
		return out.String(), errOut.String(), err
	}

	t.Run("one row", func(t *testing.T) {
		out, errOut, err := tobiraRun("query", "demo", "select * from tobira_people where id = 7", "--format", "ndjson")
		if err != nil {
			t.Fatalf("tobira query: %v\nstderr: %s", err, errOut)
		}
		rows := ndjson(t, out)
		if len(rows) != 1 {
			t.Fatalf("got %d rows, want 1:\n%s", len(rows), out)
		}
		want := drivertest.People.Rows[6] // id 7
		if got := rows[0]["id"]; got != float64(7) {
			t.Errorf("id = %v, want 7", got)
		}
		if name, _ := want[1].AsText(); rows[0]["name"] != name {
			t.Errorf("name = %v, want %q", rows[0]["name"], name)
		}
	})

	t.Run("every row", func(t *testing.T) {
		out, errOut, err := tobiraRun("query", "demo", "select * from tobira_people", "--format", "ndjson")
		if err != nil {
			t.Fatalf("tobira query: %v\nstderr: %s", err, errOut)
		}
		rows := ndjson(t, out)
		if len(rows) != len(drivertest.People.Rows) {
			t.Fatalf("got %d rows, want %d", len(rows), len(drivertest.People.Rows))
		}
		var ids []float64
		for _, r := range rows {
			ids = append(ids, r["id"].(float64))
		}
		if !slices.IsSorted(ids) || ids[0] != 1 {
			t.Errorf("ids %v, want 1..%d in order", ids, len(drivertest.People.Rows))
		}
	})

	t.Run("read-only connection", func(t *testing.T) {
		_, errOut, err := tobiraRun("query", "demo-ro", "delete from tobira_people")
		if err == nil {
			t.Fatal("a delete on a read-only connection succeeded")
		}
		// Exit code 4 is tobira's read-only refusal.
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 4 {
			t.Errorf("exit = %v, want exit status 4 (read-only refusal)", err)
		}
		if !strings.Contains(errOut, "read-only") {
			t.Errorf("stderr does not explain the refusal: %q", errOut)
		}
	})

	want := memdemo.New().Info()
	pluginPath, err := filepath.EvalSymlinks(filepath.Join(pluginDir, pluginFile()))
	if err != nil {
		t.Fatal(err)
	}
	type listRow struct {
		Name, Kind, Version, Source, Status string
		Capabilities                        []string
	}
	list := func(t *testing.T, args ...string) ([]listRow, string) {
		t.Helper()
		out, errOut, err := tobiraRun(append([]string{"driver", "list"}, args...)...)
		if err != nil {
			t.Fatalf("tobira driver list: %v\nstderr: %s", err, errOut)
		}
		var rows []listRow
		if err := json.Unmarshal([]byte(out), &rows); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, out)
		}
		return rows, errOut
	}
	named := func(rows []listRow, name string) []listRow {
		var out []listRow
		for _, r := range rows {
			if r.Name == name {
				out = append(out, r)
			}
		}
		return out
	}

	t.Run("driver list", func(t *testing.T) {
		rows, errOut := list(t)
		got := named(rows, "memdemo")
		if len(got) == 0 {
			t.Fatalf("driver list has no memdemo: %+v", rows)
		}
		m := got[0]
		if m.Source != "plugin:"+pluginPath || m.Status != "ok" || m.Kind != string(want.Kind) ||
			m.Version != want.Version || len(m.Capabilities) != len(want.Capabilities) {
			t.Errorf("memdemo row = %+v, want the plugin's real Info %+v", m, want)
		}
		if s := named(rows, "sqlite"); len(s) == 0 || s[0].Source != "builtin" {
			t.Errorf("sqlite rows = %+v, want the built-in first", s)
		}
		if runtime.GOOS != "windows" {
			if len(got) != 2 || !strings.HasPrefix(got[1].Status, "shadowed: shadowed by "+pluginPath) {
				t.Errorf("memdemo rows = %+v, want the PATH copy shadowed by the plugins dir", got)
			}
			if s := named(rows, "sqlite"); len(s) != 2 || !strings.HasPrefix(s[1].Status, "shadowed: ") {
				t.Errorf("sqlite rows = %+v, want the PATH plugin shadowed by the built-in", s)
			}
			if !strings.Contains(errOut, `warning: plugin "sqlite"`) || !strings.Contains(errOut, `warning: plugin "memdemo"`) {
				t.Errorf("stderr lacks the conflict warnings: %q", errOut)
			}
		}
	})

	t.Run("driver list --no-start", func(t *testing.T) {
		rows, _ := list(t, "--no-start")
		got := named(rows, "memdemo")
		if len(got) == 0 || got[0].Status != "not started" || got[0].Kind != "other" || len(got[0].Capabilities) != 0 {
			t.Errorf("memdemo rows = %+v, want the unstarted stub", got)
		}
	})

	t.Run("driver info", func(t *testing.T) {
		out, errOut, err := tobiraRun("driver", "info", "memdemo")
		if err != nil {
			t.Fatalf("tobira driver info: %v\nstderr: %s", err, errOut)
		}
		var info struct {
			Name, Status, Dialect, Path, SHA256 string
			Schemes                             []string
			Capabilities                        []struct {
				Name      string
				Supported bool
			}
			Conflicts []string
		}
		if err := json.Unmarshal([]byte(out), &info); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, out)
		}
		body, err := os.ReadFile(pluginPath)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		if info.Name != "memdemo" || info.Status != "ok" || info.Dialect != want.Dialect || info.Path != pluginPath ||
			info.SHA256 != hex.EncodeToString(sum[:]) || !slices.Equal(info.Schemes, want.Schemes) {
			t.Errorf("info = %+v, want memdemo's Info, path and sha256", info)
		}
		for _, c := range info.Capabilities {
			if c.Supported != slices.Contains(want.Capabilities, driver.Capability(c.Name)) {
				t.Errorf("matrix: %s supported = %v", c.Name, c.Supported)
			}
		}
		if runtime.GOOS != "windows" && len(info.Conflicts) != 1 {
			t.Errorf("conflicts = %q, want the shadowed PATH copy", info.Conflicts)
		}
	})
}

// ndjson decodes one JSON object per line.
func ndjson(t *testing.T, out string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatalf("not ndjson: %v: %q", err, sc.Text())
		}
		rows = append(rows, row)
	}
	return rows
}
