//go:build aifake

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// tobira ask --answer over a plugin connection: the plugin's real Info is
// loaded before the turn, so the model's first query and sample calls run
// rather than being refused as not read-only. A plugin that never answers
// is a connection error before any model call.
func TestAskEndToEndPlugin(t *testing.T) {
	bin := askBinary(t)
	dir := t.TempDir()
	env := pluginEnv(t, isolatedEnv(t), "ai:\n  provider: fake\n")
	script := `[
  {"tool_requests":[
    {"name":"query","input":{"connection":"plug","query":"select * from tobira_people where id = 7"}},
    {"name":"sample","input":{"connection":"plug","collection":"tobira_people","limit":2}}
  ]},
  {"text":"Margaret is there.","usage":{"input_tokens":50,"output_tokens":5}}
]`
	scriptPath := filepath.Join(dir, "script.json")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(dir, "requests.jsonl")
	env = withEnv(env, "TOBIRA_AI_FAKE_SCRIPT="+scriptPath)
	env = withEnv(env, "TOBIRA_AI_FAKE_CAPTURE="+capture)

	out, errOut, code := ask(t, bin, env, "ask", "plug", "--answer", "is Margaret there?")
	if code != 0 {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	for _, w := range []string{"Margaret is there.", "select * from tobira_people where id = 7 (1 row)"} {
		if !strings.Contains(out, w) {
			t.Errorf("stdout missing %q:\n%s", w, out)
		}
	}
	if reqs := captured(t, capture); strings.Contains(reqs, "read-only queries") {
		t.Errorf("a read tool was refused on the plugin connection:\n%s", reqs)
	}

	if runtime.GOOS == "windows" {
		return
	}
	_ = os.Remove(capture)
	out, errOut, code = ask(t, bin, env, "ask", "gone", "anything")
	if code != 2 || !strings.Contains(errOut, `connection "gone" is unavailable: driver plugin "gone" did not answer`) {
		t.Errorf("ask gone: exit %d, stdout %q, stderr %q; want 2 saying gone is unavailable", code, out, errOut)
	}
	if reqs := captured(t, capture); reqs != "" {
		t.Errorf("the model was called for an unavailable connection:\n%s", reqs)
	}
}
