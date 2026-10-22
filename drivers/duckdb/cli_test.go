package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/xavidop/tobira/drivers/duckdb/internal/duckdb"
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

// TestInstallAndQueryThroughTobira is the path a user takes: download the
// release binary with `tobira driver install <url> --sha256 …`, then query a
// duckdb:// connection with the real tobira binary.
func TestInstallAndQueryThroughTobira(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the e2e test uses POSIX permissions on the plugins directory")
	}
	built := filepath.Join(buildPlugin(t), pluginFile())
	body, err := os.ReadFile(built)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	// The release publishes the binary under a versioned name, as
	// .goreleaser.yaml's archive name_template spells it.
	versioned := "tobira-driver-duckdb_0.1.0_" + runtime.GOOS + "_" + runtime.GOARCH
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases/"+pluginFile() && r.URL.Path != "/releases/"+versioned {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	tobira := buildTobira(t)
	db := seedFile(t)
	home := t.TempDir()
	pluginDir := filepath.Join(home, "plugins") // created 0700 by install
	cfg := filepath.Join(home, "config.yaml")
	config := "plugins:\n  dirs:\n    - " + pluginDir + "\n" +
		"connections:\n" +
		"  - name: duck\n    dsn: duckdb://" + db + "\n" +
		"  - name: duck-ro\n    dsn: duckdb://" + db + "\n    read_only: true\n"
	if err := os.WriteFile(cfg, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home,
		"XDG_CONFIG_HOME=" + home, "XDG_STATE_HOME=" + home, "XDG_DATA_HOME=" + home,
	}
	run := func(args ...string) (stdout, stderr string, err error) {
		var out, errOut bytes.Buffer
		cmd := exec.Command(tobira, append([]string{"--config", cfg}, args...)...)
		cmd.Env, cmd.Stdout, cmd.Stderr = env, &out, &errOut
		err = cmd.Run()
		return out.String(), errOut.String(), err
	}

	url := srv.URL + "/releases/" + pluginFile()
	t.Run("wrong sha256 is refused", func(t *testing.T) {
		_, errOut, err := run("driver", "install", url, "--sha256", strings.Repeat("0", 64))
		if err == nil {
			t.Fatal("install with a wrong checksum succeeded")
		}
		if _, statErr := os.Stat(filepath.Join(pluginDir, pluginFile())); statErr == nil {
			t.Fatalf("a binary with a wrong checksum was installed (stderr %q)", errOut)
		}
	})

	out, errOut, err := run("driver", "install", url, "--sha256", sha)
	if err != nil {
		t.Fatalf("tobira driver install: %v\nstderr: %s", err, errOut)
	}
	if !strings.Contains(out, `installed plugin "duckdb"`) {
		t.Errorf("install output = %q", out)
	}

	t.Run("driver info", func(t *testing.T) {
		out, errOut, err := run("driver", "info", duckdb.Name)
		if err != nil {
			t.Fatalf("tobira driver info: %v\nstderr: %s", err, errOut)
		}
		var info struct{ Name, Status, SHA256 string }
		if err := json.Unmarshal([]byte(out), &info); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, out)
		}
		if info.Name != duckdb.Name || info.Status != "ok" || info.SHA256 != sha {
			t.Errorf("info = %+v, want the installed plugin, ok, sha %s", info, sha)
		}
	})

	t.Run("install from the versioned release name with --name", func(t *testing.T) {
		out, errOut, err := run("driver", "install", srv.URL+"/releases/"+versioned, "--sha256", sha, "--name", "duckdb", "--force")
		if err != nil {
			t.Fatalf("tobira driver install --name duckdb: %v\nstderr: %s", err, errOut)
		}
		if !strings.Contains(out, `installed plugin "duckdb"`) {
			t.Errorf("install output = %q", out)
		}
		if _, err := os.Stat(filepath.Join(pluginDir, pluginFile())); err != nil {
			t.Errorf("the plugin is not installed as %s: %v", pluginFile(), err)
		}
	})

	t.Run("query", func(t *testing.T) {
		out, errOut, err := run("query", "duck", "select id, name, score from tobira_people where id = 7", "--format", "ndjson")
		if err != nil {
			t.Fatalf("tobira query: %v\nstderr: %s", err, errOut)
		}
		rows := ndjson(t, out)
		if len(rows) != 1 || rows[0]["id"] != float64(7) {
			t.Fatalf("rows = %v, want id 7", rows)
		}
	})

	t.Run("read-only connection", func(t *testing.T) {
		_, errOut, err := run("query", "duck-ro", "copy tobira_people to 'out.csv'")
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 4 {
			t.Errorf("exit = %v, want exit status 4 (read-only refusal); stderr %q", err, errOut)
		}
		out, errOut, err := run("query", "duck-ro", "select count(*) as n from tobira_people", "--format", "ndjson")
		if err != nil {
			t.Fatalf("read on the read-only connection: %v\nstderr: %s", err, errOut)
		}
		if rows := ndjson(t, out); len(rows) != 1 || rows[0]["n"] != float64(57) {
			t.Errorf("rows = %v, want n = 57", rows)
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
