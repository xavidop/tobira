//go:build aifake

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const rowSecret = "zebra-row-9137"

// askBinary builds tobira with the scripted fake provider.
func askBinary(t *testing.T) string {
	t.Helper()
	return sharedBuild(t, "aifake")
}

// askEnv sets up a sqlite database with one row, a config with the fake
// provider and the script it answers from. It returns the environment and
// the file the model requests are captured in.
func askEnv(t *testing.T, bin, script string, aiExtra ...string) (env []string, capture string) {
	t.Helper()
	dir := t.TempDir()
	env = append(os.Environ(), "HOME="+dir, "XDG_CONFIG_HOME="+dir, "XDG_STATE_HOME="+dir)
	db := filepath.Join(dir, "shop.db")
	for _, q := range []string{
		"create table pets (id integer primary key, name text)",
		"insert into pets (name) values ('" + rowSecret + "')",
	} {
		if out, code := run(t, bin, env, "query", "sqlite:"+db, q); code != 0 {
			t.Fatalf("%s: exit %d (%s)", q, code, out)
		}
	}
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("connections:\n  - name: shop\n    driver: sqlite\n    dsn: sqlite:"+db+"\nai:\n  provider: fake\n"+strings.Join(aiExtra, "")), 0o600); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(dir, "script.json")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	capture = filepath.Join(dir, "requests.jsonl")
	env = append(env, "TOBIRA_CONFIG="+cfg, "TOBIRA_AI_FAKE_SCRIPT="+scriptPath, "TOBIRA_AI_FAKE_CAPTURE="+capture)
	return env, capture
}

func captured(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

// ask runs the binary and returns stdout, stderr and the exit code.
func ask(t *testing.T, bin string, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return out.String(), errOut.String(), 0
	case errors.As(err, &ee):
		return out.String(), errOut.String(), ee.ExitCode()
	}
	t.Fatalf("run %v: %v", args, err)
	return "", "", -1
}

func TestAskEndToEndProposesTheScriptedQuery(t *testing.T) {
	bin := askBinary(t)
	env, capture := askEnv(t, bin, `[{"json":{"query":"select name from pets","explanation":"Lists the pet names."},"usage":{"input_tokens":120,"output_tokens":12}}]`)
	out, errOut, code := ask(t, bin, env, "ask", "list the pets", "--format", "json")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	var got struct {
		Mode, Query, Explanation string
		Usage                    struct {
			Model string `json:"model"`
			In    int    `json:"input_tokens"`
			Out   int    `json:"output_tokens"`
		}
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if got.Mode != "ask" || got.Query != "select name from pets" || got.Explanation != "Lists the pet names." ||
		got.Usage.Model != "fake/scripted" || got.Usage.In != 120 || got.Usage.Out != 12 {
		t.Errorf("result = %+v", got)
	}
	if !strings.Contains(captured(t, capture), "list the pets") {
		t.Errorf("the question never reached the model: %s", captured(t, capture))
	}
}

const answerScript = `[
  {"tool_requests":[{"name":"query","input":{"connection":"shop","query":"select name from pets"}}]},
  {"text":"There is one pet.","usage":{"input_tokens":50,"output_tokens":5}}
]`

func TestAskEndToEndAnswerRunsAScriptedQuery(t *testing.T) {
	bin := askBinary(t)
	env, capture := askEnv(t, bin, answerScript)
	out, errOut, code := ask(t, bin, env, "ask", "--answer", "how many pets?")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	for _, w := range []string{"There is one pet.", "Queries run:", "select name from pets (1 row)"} {
		if !strings.Contains(out, w) {
			t.Errorf("stdout missing %q:\n%s", w, out)
		}
	}
	// Rows are withheld without --send-rows: the model's second request holds
	// the tool result but not the row.
	if reqs := captured(t, capture); !strings.Contains(reqs, "rows_withheld") || strings.Contains(reqs, rowSecret) {
		t.Errorf("without --send-rows the requests must withhold rows:\n%s", reqs)
	}
}

func TestAskEndToEndSendRowsSendsTheRow(t *testing.T) {
	bin := askBinary(t)
	env, capture := askEnv(t, bin, answerScript)
	if out, errOut, code := ask(t, bin, env, "ask", "--answer", "--send-rows", "how many pets?"); code != 0 {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if reqs := captured(t, capture); !strings.Contains(reqs, rowSecret) {
		t.Errorf("with --send-rows the row must reach the model:\n%s", reqs)
	}
}

func sentRowsUnder(t *testing.T, policy string, args ...string) (rowSent bool, withheld bool, out, errOut string, code int) {
	t.Helper()
	bin := askBinary(t)
	env, capture := askEnv(t, bin, answerScript, "  send_rows: "+policy+"\n")
	out, errOut, code = ask(t, bin, env, append([]string{"ask", "--answer"}, append(args, "how many pets?")...)...)
	reqs := captured(t, capture)
	return strings.Contains(reqs, rowSecret), strings.Contains(reqs, "rows_withheld"), out, errOut, code
}

func TestAskEndToEndNeverWithholdsWithoutTheFlag(t *testing.T) {
	sent, withheld, _, errOut, code := sentRowsUnder(t, "never")
	if code != 0 || sent || !withheld {
		t.Errorf("exit %d, row sent %v, withheld %v, stderr %q; want a normal run that withholds rows", code, sent, withheld, errOut)
	}
}

func TestAskEndToEndNeverRefusesSendRows(t *testing.T) {
	sent, _, out, errOut, code := sentRowsUnder(t, "never", "--send-rows")
	if code != 4 || sent || out != "" || !strings.Contains(errOut, "ai.send_rows is never") {
		t.Errorf("exit %d, row sent %v, stdout %q, stderr %q; want a policy refusal (exit 4)", code, sent, out, errOut)
	}
}

func TestAskEndToEndAlwaysSendsWithoutTheFlag(t *testing.T) {
	sent, _, _, errOut, code := sentRowsUnder(t, "always")
	if code != 0 || !sent {
		t.Errorf("exit %d, row sent %v, stderr %q; want the row sent", code, sent, errOut)
	}
}

// ask --run runs the proposal read-only on the real sqlite driver: the
// result is printed, the model never sees it, and a proposal that writes
// is refused with exit 4 and changes nothing.
func TestAskEndToEndRunRunsTheProposalReadOnly(t *testing.T) {
	bin := askBinary(t)
	env, capture := askEnv(t, bin, `[{"json":{"query":"select name from pets","explanation":"Lists the pet names."}}]`)
	out, errOut, code := ask(t, bin, env, "ask", "shop", "list the pets", "--run", "--format", "csv")
	if code != 0 || out != "name\n"+rowSecret+"\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if !strings.Contains(errOut, "select name from pets") {
		t.Errorf("the proposal is not on stderr: %q", errOut)
	}
	if strings.Contains(captured(t, capture), rowSecret) {
		t.Error("the result of --run reached the model")
	}
	env, _ = askEnv(t, bin, `[{"json":{"query":"delete from pets","explanation":"Deletes every pet."}}]`)
	if out, errOut, code := ask(t, bin, env, "ask", "shop", "remove the pets", "--run"); code != 4 {
		t.Fatalf("write proposal: exit %d, stdout %q, stderr %q; want 4", code, out, errOut)
	}
	if out, code := run(t, bin, env, "query", "shop", "select count(*) as n from pets", "--format", "csv"); code != 0 || out != "n\n1\n" {
		t.Fatalf("after the refused write: exit %d, %q", code, out)
	}
}
