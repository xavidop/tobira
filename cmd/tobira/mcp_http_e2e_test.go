package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

var (
	httpListenLine = regexp.MustCompile(`^MCP HTTP server listening on (http://\S+/mcp)$`)
	httpTokenLine  = regexp.MustCompile(`^MCP HTTP token: ([0-9a-f]{64}) \(pass as Authorization: Bearer …\)$`)
)

// startHTTP starts `tobira mcp <args>` and waits for its listening line
// (and its token line when wantToken). It returns the endpoint URL, the
// generated token, everything on stderr so far plus a function returning
// the rest, and a function that sends SIGINT and returns the exit error.
func startHTTP(t *testing.T, bin string, env []string, wantToken bool, args ...string) (url, token string, stdout *rawStdout, stderrAll func() string, interrupt func() error) {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"mcp"}, args...)...)
	cmd.Env = env
	stdout = &rawStdout{}
	cmd.Stdout = stdout
	pr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stderr := &rawStdout{}
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			_, _ = stderr.Write([]byte(sc.Text() + "\n"))
			lines <- sc.Text()
		}
		close(lines)
	}()
	exited := make(chan error, 1)
	waited := false
	deadline := time.After(30 * time.Second)
	for url == "" || (wantToken && token == "") {
		select {
		case l, ok := <-lines:
			if !ok {
				_ = cmd.Wait()
				t.Fatalf("tobira mcp exited before serving; stderr:\n%s", stderr.String())
			}
			if m := httpListenLine.FindStringSubmatch(l); m != nil {
				url = m[1]
			}
			if m := httpTokenLine.FindStringSubmatch(l); m != nil {
				token = m[1]
			}
		case <-deadline:
			_ = cmd.Process.Kill()
			t.Fatalf("tobira mcp did not start; stderr:\n%s", stderr.String())
		}
	}
	go func() {
		for range lines { // keep draining stderr
		}
		exited <- cmd.Wait()
	}()
	interrupt = func() error {
		waited = true
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-exited:
			return err
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatal("tobira mcp did not exit after SIGINT")
			return nil
		}
	}
	t.Cleanup(func() {
		if !waited {
			_ = cmd.Process.Kill()
			t.Log("tobira mcp killed at cleanup:", <-exited)
		}
	})
	return url, token, stdout, stderr.String, interrupt
}

// TestMCPEndToEndHTTP runs `tobira mcp --http 127.0.0.1:0` and drives it
// with mcp-go's Streamable HTTP client using the token printed on stderr:
// the calls work and are in history with the client's name, a request
// without the token is 401, one for a foreign Host is 403, SIGINT exits 0
// and frees the port, and nothing is written to stdout.
func TestMCPEndToEndHTTP(t *testing.T) {
	bin := buildBinary(t)
	env := isolatedEnv(t)
	path := newSQLiteFile(t, bin, env)
	if out, code := run(t, bin, env, "query", "sqlite:"+path, "insert into t (id) values (1), (2)"); code != 0 {
		t.Fatalf("seed: exit %d (%q)", code, out)
	}
	env = writeConfig(t, env, "connections:\n  - name: lite\n    driver: sqlite\n    dsn: sqlite:"+path+"\n")
	url, token, stdout, stderr, interrupt := startHTTP(t, bin, env, true, "--expose", "lite", "--http", "127.0.0.1:0")
	if n := strings.Count(stderr(), token); n != 1 {
		t.Errorf("the token is printed %d times, want once", n)
	}

	c, err := client.NewStreamableHttpClient(url, transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + token}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var init mcp.InitializeRequest
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "tobira-http-e2e", Version: "0.0.2"}
	if _, err := c.Initialize(ctx, init); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	out, isErr := mcpCall(t, c, "query", map[string]any{"connection": "lite", "query": "select id from t order by id"})
	if isErr || !strings.Contains(out, `"rows":[[1],[2]]`) {
		t.Errorf("query = %s (error %v)", out, isErr)
	}

	status := func(host string, hdr map[string]string) int {
		req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := status("", nil); code != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", code)
	}
	addr := strings.TrimSuffix(strings.TrimPrefix(url, "http://"), "/mcp")
	_, port, _ := net.SplitHostPort(addr)
	if code := status("rebound.test:"+port, map[string]string{"Authorization": "Bearer " + token}); code != http.StatusForbidden {
		t.Errorf("foreign Host: status %d, want 403", code)
	}
	if code := status("", map[string]string{"Authorization": "Bearer " + token, "Origin": "https://evil.test"}); code != http.StatusForbidden {
		t.Errorf("evil Origin: status %d, want 403", code)
	}

	if err := interrupt(); err != nil {
		t.Errorf("exit after SIGINT: %v, want 0", err)
	}
	if ln, err := net.Listen("tcp", addr); err != nil {
		t.Errorf("the port is still taken after exit: %v", err)
	} else {
		_ = ln.Close()
	}
	if s := stdout.String(); s != "" {
		t.Errorf("stdout = %q, want nothing in HTTP mode", s)
	}

	hout, code := run(t, bin, env, "history", "--mcp", "--format", "json")
	if code != 0 {
		t.Fatalf("history --mcp: exit %d (%q)", code, hout)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(hout), &rows); err != nil {
		var doc struct {
			Rows []map[string]any `json:"rows"`
		}
		if err := json.Unmarshal([]byte(hout), &doc); err != nil {
			t.Fatalf("history is not JSON: %v\n%s", err, hout)
		}
		rows = doc.Rows
	}
	if len(rows) != 1 || rows[0]["client"] != "tobira-http-e2e/0.0.2" || rows[0]["source"] != "mcp" {
		t.Errorf("history --mcp = %v, want the query with client tobira-http-e2e/0.0.2", rows)
	}
}

// A token from mcp.token is used and never printed.
func TestMCPEndToEndHTTPConfigToken(t *testing.T) {
	bin := buildBinary(t)
	env := isolatedEnv(t)
	path := newSQLiteFile(t, bin, env)
	env = writeConfig(t, env, "connections:\n  - name: lite\n    driver: sqlite\n    dsn: sqlite:"+path+"\nmcp:\n  expose: [lite]\n  token: env:TOBIRA_E2E_MCP_TOKEN\n")
	env = withEnv(env, "TOBIRA_E2E_MCP_TOKEN=e2e-config-token-31")
	url, _, _, stderr, interrupt := startHTTP(t, bin, env, false, "--http", ":0")
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Errorf("--http :0 served %s, want 127.0.0.1", url)
	}
	c, err := client.NewStreamableHttpClient(url, transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer e2e-config-token-31"}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var init mcp.InitializeRequest
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "cfg"}
	if _, err := c.Initialize(ctx, init); err != nil {
		t.Fatalf("initialize with the configured token: %v", err)
	}
	if err := interrupt(); err != nil {
		t.Errorf("exit after SIGINT: %v", err)
	}
	if s := stderr(); strings.Contains(s, "e2e-config-token-31") || strings.Contains(s, "MCP HTTP token") {
		t.Errorf("stderr shows the configured token: %q", s)
	}
}

// A non-loopback address without --insecure-bind: exit 4, nothing served.
func TestMCPEndToEndHTTPInsecureBindRefused(t *testing.T) {
	bin := buildBinary(t)
	env := isolatedEnv(t)
	path := newSQLiteFile(t, bin, env)
	env = writeConfig(t, env, "connections:\n  - name: lite\n    driver: sqlite\n    dsn: sqlite:"+path+"\nmcp:\n  expose: [lite]\n")
	stdout, stderr, code := runBoth(t, bin, env, "mcp", "--http", "0.0.0.0:0")
	if code != 4 || stdout != "" || !strings.Contains(stderr, "--insecure-bind") || strings.Contains(stderr, "listening") {
		t.Errorf("code=%d stdout=%q stderr=%q, want exit 4 naming --insecure-bind and no listener", code, stdout, stderr)
	}
}
