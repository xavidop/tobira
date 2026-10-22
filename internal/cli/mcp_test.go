package cli_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/xavidop/tobira/internal/cli"
	"github.com/xavidop/tobira/internal/mcphttp"
)

// mcpHarness is a harness whose config is testConfig plus mcpSection, with
// a ServeMCP that records the options it gets.
func mcpHarness(t *testing.T, mcpSection string) (*harness, *[]cli.MCPOptions) {
	t.Helper()
	h := newHarness(t)
	if err := os.WriteFile(h.cfg, []byte(testConfig+mcpSection), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []cli.MCPOptions
	h.serveMCP = func(_ context.Context, o cli.MCPOptions) error {
		got = append(got, o)
		return nil
	}
	return h, &got
}

func TestMCPServesWithTheConfigPolicy(t *testing.T) {
	h, got := mcpHarness(t, "mcp:\n  expose: [local-pg, ro]\n  allow_writes: [local-pg]\n  max_rows: 50\n  send_rows: never\n")
	out, errOut, code := h.exec(t, context.Background(), "mcp")
	if code != 0 || out != "" || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if len(*got) != 1 {
		t.Fatalf("ServeMCP called %d times", len(*got))
	}
	o := (*got)[0]
	if !slices.Equal(o.Policy.Expose, []string{"local-pg", "ro"}) || !slices.Equal(o.Policy.AllowWrites, []string{"local-pg"}) ||
		o.Policy.MaxRows != 50 || o.Policy.SendRows != "never" {
		t.Errorf("policy = %+v", o.Policy)
	}
	if o.Manager == nil || o.Stdin == nil || o.Stdout == nil || o.Logger == nil {
		t.Errorf("options missing collaborators: %+v", o)
	}
}

func TestMCPFlagsReplaceTheConfig(t *testing.T) {
	h, got := mcpHarness(t, "mcp:\n  expose: [local-pg]\n  allow_writes: [local-pg]\n  max_rows: 50\n")
	_, errOut, code := h.exec(t, context.Background(), "mcp", "--expose", "ro,local-pg", "--allow-writes", "ro", "--max-rows", "1000", "--send-rows", "never")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	o := (*got)[0]
	if !slices.Equal(o.Policy.Expose, []string{"ro", "local-pg"}) || !slices.Equal(o.Policy.AllowWrites, []string{"ro"}) ||
		o.Policy.MaxRows != 1000 || o.Policy.SendRows != "never" {
		t.Errorf("policy = %+v", o.Policy)
	}
}

func TestMCPDefaultsWithoutASection(t *testing.T) {
	h, got := mcpHarness(t, "")
	if _, errOut, code := h.exec(t, context.Background(), "mcp", "--expose", "local-pg"); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	o := (*got)[0]
	if o.Policy.MaxRows != 200 || o.Policy.SendRows != "always" || len(o.Policy.AllowWrites) != 0 {
		t.Errorf("policy = %+v", o.Policy)
	}
}

func TestMCPRefusesToStart(t *testing.T) {
	for _, tc := range []struct {
		name, section string
		args          []string
		want          string
	}{
		{"nothing exposed", "", nil, "nothing is exposed: pass --expose or set mcp.expose"},
		{"empty expose list", "mcp:\n  expose: []\n", nil, "nothing is exposed"},
		{"empty flag", "mcp:\n  expose: [local-pg]\n", []string{"--expose", ""}, "nothing is exposed"},
		{"writes outside expose (config)", "mcp:\n  expose: [local-pg]\n  allow_writes: [ro]\n", nil, "mcp.allow_writes[0]"},
		{"writes outside expose (flag)", "", []string{"--expose", "local-pg", "--allow-writes", "ro"}, "--allow-writes[0]"},
		{"unknown name (config)", "mcp:\n  expose: [local-pg, nosuch]\n", nil, "mcp.expose[1]: no connection with that name"},
		{"unknown name (flag)", "", []string{"--expose", "nosuch"}, "--expose[0]: no connection with that name"},
		{"unknown after a duplicate (config)", "mcp:\n  expose: [local-pg, local-pg, ghost]\n", nil, "mcp.expose[2]: no connection with that name"},
		{"unknown after a duplicate (flag)", "", []string{"--expose", "local-pg,local-pg,ghost"}, "--expose[2]: no connection with that name"},
		{"max rows zero", "", []string{"--expose", "local-pg", "--max-rows", "0"}, "--max-rows"},
		{"max rows over cap", "", []string{"--expose", "local-pg", "--max-rows", "1001"}, "--max-rows"},
		{"bad send rows", "", []string{"--expose", "local-pg", "--send-rows", "ask"}, "--send-rows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, got := mcpHarness(t, tc.section)
			out, errOut, code := h.exec(t, context.Background(), append([]string{"mcp"}, tc.args...)...)
			if code != 1 {
				t.Fatalf("code=%d, want 1 (stderr %q)", code, errOut)
			}
			if out != "" {
				t.Errorf("stdout = %q, want nothing (it is the protocol stream)", out)
			}
			if !strings.Contains(errOut, tc.want) {
				t.Errorf("stderr = %q, want it to contain %q", errOut, tc.want)
			}
			if len(*got) != 0 {
				t.Error("ServeMCP was called")
			}
		})
	}
}

// An unknown name is never echoed: it could be a DSN typed in the wrong
// place.
func TestMCPDoesNotEchoUnknownNames(t *testing.T) {
	h, _ := mcpHarness(t, "")
	_, errOut, _ := h.exec(t, context.Background(), "mcp", "--expose", "postgres://u:hunter2@db/x")
	if strings.Contains(errOut, "hunter2") || strings.Contains(errOut, "postgres://") {
		t.Errorf("stderr echoes the value: %q", errOut)
	}
}

func TestMCPPrintsConfigWarningsToStderr(t *testing.T) {
	h, got := mcpHarness(t, "mcp:\n  expose: [local-pg, local-pg]\n")
	out, errOut, code := h.exec(t, context.Background(), "mcp")
	if code != 0 || out != "" {
		t.Fatalf("code=%d stdout=%q", code, out)
	}
	if !strings.Contains(errOut, "warning: mcp.expose[1]: duplicate entry, ignored") {
		t.Errorf("stderr = %q, want the config warning", errOut)
	}
	if !slices.Equal((*got)[0].Policy.Expose, []string{"local-pg"}) {
		t.Errorf("expose = %v", (*got)[0].Policy.Expose)
	}
}

func TestMCPWithoutAServerInTheBinary(t *testing.T) {
	h, _ := mcpHarness(t, "mcp:\n  expose: [local-pg]\n")
	h.serveMCP = nil
	out, errOut, code := h.exec(t, context.Background(), "mcp")
	if code != 1 || out != "" || !strings.Contains(errOut, "no MCP server") {
		t.Errorf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

func TestMCPServeFailureExits2(t *testing.T) {
	h, _ := mcpHarness(t, "mcp:\n  expose: [local-pg]\n")
	h.serveMCP = func(context.Context, cli.MCPOptions) error { return errors.New("stdout closed") }
	out, errOut, code := h.exec(t, context.Background(), "mcp")
	if code != 2 || out != "" || !strings.Contains(errOut, "stdout closed") {
		t.Errorf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

// A signal cancels the context; the server returns and the command exits 0.
func TestMCPCancelledExits0(t *testing.T) {
	h, _ := mcpHarness(t, "mcp:\n  expose: [local-pg]\n")
	ctx, cancel := context.WithCancel(context.Background())
	h.serveMCP = func(ctx context.Context, _ cli.MCPOptions) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}
	if _, errOut, code := h.exec(t, ctx, "mcp"); code != 0 {
		t.Errorf("code=%d stderr=%q", code, errOut)
	}
}

func TestMCPStdioByDefault(t *testing.T) {
	h, got := mcpHarness(t, "mcp:\n  expose: [local-pg]\n")
	if _, errOut, code := h.exec(t, context.Background(), "mcp"); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if (*got)[0].HTTP != nil {
		t.Errorf("HTTP = %+v without --http", (*got)[0].HTTP)
	}
}

func TestMCPHTTPOptions(t *testing.T) {
	h, got := mcpHarness(t, "mcp:\n  expose: [local-pg]\n")
	out, errOut, code := h.exec(t, context.Background(), "mcp", "--http", ":8765", "--allowed-host", "Box.LAN", "--allowed-host", "proxy:8443")
	if code != 0 || out != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	o := (*got)[0]
	if o.HTTP == nil {
		t.Fatal("HTTP not set with --http")
	}
	if o.HTTP.Addr != "127.0.0.1:8765" || o.HTTP.InsecureBind || !slices.Equal(o.HTTP.AllowedHosts, []string{"box.lan", "proxy:8443"}) || !o.HTTP.Token.IsZero() {
		t.Errorf("HTTP = %+v", *o.HTTP)
	}
	if o.Stderr == nil {
		t.Error("no Stderr for the address and token")
	}
}

func TestMCPHTTPInsecureBind(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8765", "[::]:8765", "192.168.1.5:8765", "box.lan:8765"} {
		h, got := mcpHarness(t, "mcp:\n  expose: [local-pg]\n")
		out, errOut, code := h.exec(t, context.Background(), "mcp", "--http", addr)
		if code != 4 || out != "" || !strings.Contains(errOut, "--insecure-bind") {
			t.Errorf("%s: code=%d stdout=%q stderr=%q, want exit 4 naming --insecure-bind", addr, code, out, errOut)
		}
		if len(*got) != 0 {
			t.Errorf("%s: ServeMCP was called", addr)
		}
	}
	h, got := mcpHarness(t, "mcp:\n  expose: [local-pg]\n")
	if _, errOut, code := h.exec(t, context.Background(), "mcp", "--http", "0.0.0.0:8765", "--insecure-bind"); code != 0 {
		t.Fatalf("with --insecure-bind: code=%d stderr=%q", code, errOut)
	}
	if o := (*got)[0].HTTP; o == nil || o.Addr != "0.0.0.0:8765" || !o.InsecureBind {
		t.Errorf("HTTP = %+v", o)
	}
}

// The server refusing a bind (its own check) is exit 4 too.
func TestMCPHTTPServerRefusalExits4(t *testing.T) {
	h, _ := mcpHarness(t, "mcp:\n  expose: [local-pg]\n")
	h.serveMCP = func(context.Context, cli.MCPOptions) error { return mcphttp.ErrInsecureBind }
	if _, errOut, code := h.exec(t, context.Background(), "mcp", "--http", "127.0.0.1:0"); code != 4 {
		t.Errorf("code=%d stderr=%q, want 4", code, errOut)
	}
}

func TestMCPHTTPUsageErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"insecure-bind without http", []string{"--insecure-bind"}, "--insecure-bind needs --http"},
		{"allowed-host without http", []string{"--allowed-host", "box"}, "--allowed-host needs --http"},
		{"bad address", []string{"--http", "8765"}, "--http: want host:port"},
		{"bad port", []string{"--http", "127.0.0.1:99999"}, "--http: the port"},
		{"bad allowed host", []string{"--http", ":0", "--allowed-host", "a/b"}, "--allowed-host"},
		{"empty allowed host", []string{"--http", ":0", "--allowed-host", ""}, "--allowed-host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, got := mcpHarness(t, "mcp:\n  expose: [local-pg]\n")
			out, errOut, code := h.exec(t, context.Background(), append([]string{"mcp"}, tc.args...)...)
			if code != 1 || out != "" || !strings.Contains(errOut, tc.want) {
				t.Errorf("code=%d stdout=%q stderr=%q, want exit 1 with %q", code, out, errOut, tc.want)
			}
			if len(*got) != 0 {
				t.Error("ServeMCP was called")
			}
		})
	}
}

func TestMCPHTTPTokenFromConfig(t *testing.T) {
	h, got := mcpHarness(t, "mcp:\n  expose: [local-pg]\n  token: env:TOBIRA_TEST_MCP_TOKEN\n")
	t.Setenv("TOBIRA_TEST_MCP_TOKEN", "tok-from-env-55\n") // env refs read the process environment
	out, errOut, code := h.exec(t, context.Background(), "mcp", "--http", "127.0.0.1:0")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if tok := (*got)[0].HTTP.Token.Reveal(); tok != "tok-from-env-55" {
		t.Errorf("token = %q", tok)
	}
	if strings.Contains(out+errOut, "tok-from-env-55") {
		t.Errorf("the configured token was printed: stdout=%q stderr=%q", out, errOut)
	}
}

func TestMCPHTTPUnresolvableTokenExits1(t *testing.T) {
	h, got := mcpHarness(t, "mcp:\n  expose: [local-pg]\n  token: env:TOBIRA_TEST_MCP_NOPE\n")
	out, errOut, code := h.exec(t, context.Background(), "mcp", "--http", "127.0.0.1:0")
	if code != 1 || out != "" || !strings.Contains(errOut, "mcp.token") {
		t.Errorf("code=%d stdout=%q stderr=%q, want exit 1 naming mcp.token", code, out, errOut)
	}
	if strings.Contains(errOut, "TOBIRA_TEST_MCP_NOPE") {
		t.Errorf("stderr names the ref: %q", errOut)
	}
	if len(*got) != 0 {
		t.Error("ServeMCP was called")
	}
	// Over stdio the token is not needed, so it is not resolved.
	if _, errOut, code := h.exec(t, context.Background(), "mcp"); code != 0 {
		t.Errorf("stdio with an unresolvable token: code=%d stderr=%q", code, errOut)
	}
}

// A configured token is trimmed: a client's bearer value is, so spaces
// around the stored one could never match.
func TestMCPHTTPTokenIsTrimmed(t *testing.T) {
	h, got := mcpHarness(t, "mcp:\n  expose: [local-pg]\n  token: env:TOBIRA_TEST_MCP_TOKEN\n")
	t.Setenv("TOBIRA_TEST_MCP_TOKEN", "  tok-with-spaces \t\n")
	if _, errOut, code := h.exec(t, context.Background(), "mcp", "--http", "127.0.0.1:0"); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if tok := (*got)[0].HTTP.Token.Reveal(); tok != "tok-with-spaces" {
		t.Errorf("token = %q, want it trimmed", tok)
	}
}

// A token that resolves to nothing but spaces is refused, never replaced
// by a generated one.
func TestMCPHTTPBlankTokenIsRefused(t *testing.T) {
	h, got := mcpHarness(t, "mcp:\n  expose: [local-pg]\n  token: env:TOBIRA_TEST_MCP_TOKEN\n")
	t.Setenv("TOBIRA_TEST_MCP_TOKEN", " \t \n")
	out, errOut, code := h.exec(t, context.Background(), "mcp", "--http", "127.0.0.1:0")
	if code != 1 || out != "" || !strings.Contains(errOut, "mcp.token resolved to an empty value") {
		t.Errorf("code=%d stdout=%q stderr=%q, want exit 1: mcp.token resolved to an empty value", code, out, errOut)
	}
	if len(*got) != 0 {
		t.Error("ServeMCP was called")
	}
}

// The MCP docs say what the config loader does: a repeated entry is warned
// about and ignored (it never stops the server), and mcp.token must be a
// secret reference (a literal token is a load error).
func TestMCPDocsMatchTheConfigRules(t *testing.T) {
	for _, file := range []string{"site/src/pages/docs/mcp.md", "site/src/pages/docs/configuration.md"} {
		text := strings.Join(strings.Fields(repoFile(t, file)), " ")
		if !strings.Contains(text, "repeated entry is warned about and ignored") {
			t.Errorf("%s does not say a repeated entry is warned about and ignored", file)
		}
		if strings.Contains(text, "a repeated entry are reported as warnings") || strings.Contains(text, "and a repeated entry do not stop") {
			t.Errorf("%s still groups repeated entries with the problems that stop the server", file)
		}
	}
	mcp := strings.Join(strings.Fields(repoFile(t, "site/src/pages/docs/mcp.md")), " ")
	if strings.Contains(mcp, "A token in a config file") {
		t.Error("mcp.md suggests a literal token can sit in the config file")
	}
	if !strings.Contains(mcp, "a literal token is a load error") {
		t.Error("mcp.md does not say a literal mcp.token is a load error")
	}
}
