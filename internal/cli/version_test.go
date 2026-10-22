package cli_test

import (
	"bytes"
	"context"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/xavidop/tobira/internal/cli"
)

var update = flag.Bool("update", false, "update golden files")

func run(t *testing.T, info cli.BuildInfo, args ...string) (stdout string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := cli.NewRoot(info, cli.Streams{In: strings.NewReader(""), Out: &out, Err: &errOut}, cli.Deps{})
	code = cli.Execute(context.Background(), root, args)
	return out.String(), code
}

func TestVersion(t *testing.T) {
	got, code := run(t, cli.BuildInfo{Version: "1.2.3", Commit: "abc123", Date: "2026-10-19"}, "version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	const golden = "testdata/version.golden"
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestVersionDev(t *testing.T) {
	got, code := run(t, cli.BuildInfo{}, "version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if want := "tobira dev (none, unknown)\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestBareRootPrintsHelp(t *testing.T) {
	got, code := run(t, cli.BuildInfo{})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	for _, s := range []string{"Every database, one terminal.", "open the interactive browser"} {
		if !strings.Contains(got, s) {
			t.Errorf("stdout missing %q:\n%s", s, got)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	_, code := run(t, cli.BuildInfo{}, "bogus")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}
