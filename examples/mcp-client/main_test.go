package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildTobira builds the CLI module next to this one into a temp dir.
func buildTobira(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "tobira")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = filepath.Join("..", "..", "cmd", "tobira")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build tobira: %v\n%s", err, out)
	}
	return bin
}

// TestExampleAgainstRealTobira builds tobira, points it at a temp SQLite
// database and runs the example against it.
func TestExampleAgainstRealTobira(t *testing.T) {
	bin := buildTobira(t)
	dir := t.TempDir()
	env := append(os.Environ(),
		"HOME="+dir, "XDG_CONFIG_HOME="+dir, "XDG_STATE_HOME="+dir,
		"TOBIRA_CONFIG="+filepath.Join(dir, "missing.yaml"))

	db := filepath.Join(dir, "example.db")
	seed := exec.Command(bin, "query", "sqlite:"+db, "create table notes (id integer primary key, body text)")
	seed.Env = env
	if out, err := seed.CombinedOutput(); err != nil {
		t.Fatalf("seed: %v\n%s", err, out)
	}
	cfg := filepath.Join(dir, "config.yaml")
	body := "connections:\n  - name: lite\n    driver: sqlite\n    dsn: sqlite:" + db + "\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := run(context.Background(), &out, options{
		tobira: bin, config: cfg, connection: "lite", collection: "notes", env: env,
	})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"tools:", "  describe", "  query", "describe:", `"collection":"notes"`, `"primary_key":["id"]`} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "  execute") {
		t.Errorf("execute is offered without allow_writes:\n%s", got)
	}
}

func TestRunNeedsAConnectionAndCollection(t *testing.T) {
	if err := run(context.Background(), &bytes.Buffer{}, options{}); err == nil {
		t.Fatal("run with no options succeeded")
	}
}
