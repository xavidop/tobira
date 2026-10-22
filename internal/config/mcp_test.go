package config

import (
	"reflect"
	"strings"
	"testing"
)

const mcpConns = "connections:\n  - {name: a, driver: sqlite}\n  - {name: b, driver: sqlite}\n"

func TestMCPAbsentIsNilWithDefaults(t *testing.T) {
	for name, body := range map[string]string{
		"no section":   "connections: []\n",
		"null section": "mcp:\n",
		"empty file":   "",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := Load(writeTemp(t, body))
			if err != nil {
				t.Fatal(err)
			}
			if c.MCP != nil {
				t.Fatalf("MCP = %+v, want nil", c.MCP)
			}
			want := MCP{MaxRows: 200, SendRows: "always"}
			if got := c.MCPSettings(); !reflect.DeepEqual(got, want) {
				t.Fatalf("MCPSettings = %+v, want %+v", got, want)
			}
		})
	}
}

func TestMCPDefaultsAndFull(t *testing.T) {
	c, err := Load(writeTemp(t, "mcp: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := (&MCP{MaxRows: 200, SendRows: "always"}); !reflect.DeepEqual(c.MCP, want) {
		t.Fatalf("MCP = %+v, want %+v", c.MCP, want)
	}
	body := mcpConns + `mcp:
  expose: [a, b]
  allow_writes: [b]
  max_rows: 1000
  send_rows: never
  token: env:TOBIRA_MCP_TOKEN
`
	c, err = Load(writeTemp(t, body))
	if err != nil {
		t.Fatal(err)
	}
	want := &MCP{Expose: []string{"a", "b"}, AllowWrites: []string{"b"}, MaxRows: 1000, SendRows: "never", Token: "env:TOBIRA_MCP_TOKEN"}
	if !reflect.DeepEqual(c.MCP, want) || !reflect.DeepEqual(c.MCPSettings(), *want) {
		t.Fatalf("MCP = %+v, want %+v", c.MCP, want)
	}
}

func TestMCPValidation(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr []string
		secret  string // must never appear in the error
	}{
		{"max_rows over cap", "mcp:\n  max_rows: 1001\n", []string{"line 2", "mcp.max_rows", "1000"}, "1001"},
		{"max_rows zero", "mcp:\n  max_rows: 0\n", []string{"mcp.max_rows"}, ""},
		{"max_rows not int", "mcp:\n  max_rows: lots\n", []string{"mcp.max_rows", "integer"}, "lots"},
		{"bad send_rows", "mcp:\n  send_rows: ask\n", []string{"mcp.send_rows", "always or never"}, ""},
		{"literal token", "mcp:\n  token: s3cr3t-token-value\n", []string{"line 2", "mcp.token", "secret reference"}, "s3cr3t-token-value"},
		{"token that looks like a ref", "mcp:\n  token: \"hunter:22secret\"\n", []string{"mcp.token"}, "22secret"},
		{"expose not a list", "mcp:\n  expose: a\n", []string{"mcp.expose", "list"}, ""},
		{"expose item not a string", "mcp:\n  expose: [[x]]\n", []string{"mcp.expose"}, ""},
		{"control in expose", mcpConns + "mcp:\n  expose: [\"a\\u0085\"]\n", []string{"mcp.expose"}, ""},
		{"unknown key", "mcp:\n  exposee: [a]\n", []string{"line 2", "did you mean \"expose\""}, ""},
		{"unknown key never quoted", "mcp:\n  sk-live-abcdef123456: x\n", []string{"unknown mcp key"}, "sk-live-abcdef123456"},
		{"not a mapping", "mcp: yes\n", []string{"line 1", "mcp must be a mapping"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTemp(t, tt.yaml)
			_, err := Load(path)
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			for _, w := range tt.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("error %q does not contain %q", err, w)
				}
			}
			// The temp path is random and may contain the digits.
			if tt.secret != "" && strings.Contains(strings.ReplaceAll(err.Error(), path, ""), tt.secret) {
				t.Fatalf("error %q echoes the value %q", err, tt.secret)
			}
		})
	}
}

func TestMCPValidRefs(t *testing.T) {
	for _, ref := range []string{"env:TOBIRA_MCP_TOKEN", "file:///run/secrets/tok", "op://dev/tobira/mcp-token"} {
		c, err := Load(writeTemp(t, "mcp:\n  token: \""+ref+"\"\n"))
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		if c.MCP.Token != ref {
			t.Fatalf("token = %q, want %q", c.MCP.Token, ref)
		}
	}
}

// A broken connection may still be named in expose: the file loads, with a
// warning, so a typo in one entry does not hide the rest.
func TestMCPExposeBrokenConnectionWarns(t *testing.T) {
	body := "connections:\n  - {name: a, driver: sqlite}\n  - name: bad\n    driver: x\n    pasword: SUPERSECRET\nmcp:\n  expose: [a, bad]\n"
	c, err := Load(writeTemp(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Broken) != 1 || c.Broken[0].Name != "bad" {
		t.Fatalf("Broken = %+v", c.Broken)
	}
	if !reflect.DeepEqual(c.MCP.Expose, []string{"a", "bad"}) {
		t.Fatalf("Expose = %v", c.MCP.Expose)
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "mcp.expose") || strings.Contains(c.Warnings[0], "SUPERSECRET") {
		t.Fatalf("Warnings = %q", c.Warnings)
	}
}

// Stale or wrong names are warnings, not load errors: every other command
// keeps working. Warnings never echo the entry (a DSN could be typed there).
func TestMCPNameProblemsAreWarnings(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string
		wantWarn    []string
		wantExpose  []string
		wantWrites  []string
		secretInMsg string
	}{
		{"not subset", mcpConns + "mcp:\n  expose: [a]\n  allow_writes: [b]\n", []string{"mcp.allow_writes[0]"}, []string{"a"}, []string{"b"}, ""},
		{"writes without expose", mcpConns + "mcp:\n  allow_writes: [a]\n", []string{"mcp.allow_writes[0]"}, nil, []string{"a"}, ""},
		{"unknown name", mcpConns + "mcp:\n  expose: [a, ghost]\n", []string{"mcp.expose[1]"}, []string{"a", "ghost"}, nil, "ghost"},
		{"dsn typed in expose", mcpConns + "mcp:\n  expose: [\"postgres://u:pa55word@h/db\"]\n", []string{"mcp.expose[0]"}, []string{"postgres://u:pa55word@h/db"}, nil, "pa55word"},
		{"duplicates", mcpConns + "mcp:\n  expose: [a, b, a]\n  allow_writes: [b, b]\n", []string{"mcp.expose[2]", "mcp.allow_writes[1]"}, []string{"a", "b"}, []string{"b"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load(writeTemp(t, tt.yaml))
			if err != nil {
				t.Fatalf("Load failed, want warnings only: %v", err)
			}
			if len(c.Warnings) != len(tt.wantWarn) {
				t.Fatalf("Warnings = %q, want %d", c.Warnings, len(tt.wantWarn))
			}
			for i, w := range tt.wantWarn {
				if !strings.Contains(c.Warnings[i], w) {
					t.Errorf("Warnings[%d] = %q, want %q", i, c.Warnings[i], w)
				}
				if tt.secretInMsg != "" && strings.Contains(c.Warnings[i], tt.secretInMsg) {
					t.Errorf("warning echoes the entry: %q", c.Warnings[i])
				}
			}
			if !reflect.DeepEqual(c.MCP.Expose, tt.wantExpose) || !reflect.DeepEqual(c.MCP.AllowWrites, tt.wantWrites) {
				t.Errorf("Expose=%v AllowWrites=%v, want %v %v", c.MCP.Expose, c.MCP.AllowWrites, tt.wantExpose, tt.wantWrites)
			}
		})
	}
}

func TestMCPValidate(t *testing.T) {
	conns := []Connection{{Name: "a"}, {Name: "b"}}
	if err := (&MCP{Expose: []string{"a", "b"}, AllowWrites: []string{"b"}}).Validate(conns); err != nil {
		t.Fatalf("valid: %v", err)
	}
	for name, m := range map[string]*MCP{
		"unknown":    {Expose: []string{"a", "ghost-pa55word"}},
		"not subset": {Expose: []string{"a"}, AllowWrites: []string{"b"}},
	} {
		err := m.Validate(conns)
		if err == nil {
			t.Fatalf("%s: want an error", name)
		}
		if strings.Contains(err.Error(), "pa55word") {
			t.Errorf("%s: error echoes the entry: %v", name, err)
		}
	}
}

func TestMCPErrorIsWholeFile(t *testing.T) {
	_, err := Load(writeTemp(t, mcpConns+"mcp:\n  send_rows: nope\n"))
	if err == nil || !strings.Contains(err.Error(), "line 5") {
		t.Fatalf("err = %v, want a whole-file error at line 5", err)
	}
}

// Validate names the entry's position in the file, as the warnings do,
// not its position after duplicates were dropped.
func TestMCPValidateUsesFilePositions(t *testing.T) {
	c, err := Load(writeTemp(t, mcpConns+"mcp:\n  expose: [a, a, ghost]\n  allow_writes: [a, a, b]\n"))
	if err != nil {
		t.Fatal(err)
	}
	err = c.MCP.Validate(c.Connections)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"mcp.expose[2]: no connection", "mcp.allow_writes[2]: not in mcp.expose"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want %q", err, want)
		}
	}
	for _, wrong := range []string{"mcp.expose[1]", "mcp.allow_writes[1]"} {
		if strings.Contains(err.Error(), wrong) {
			t.Errorf("err = %q names the deduplicated position %s", err, wrong)
		}
	}
	for _, w := range c.Warnings {
		if strings.Contains(w, "no connection") && !strings.Contains(w, "mcp.expose[2]") {
			t.Errorf("warning %q disagrees with the error", w)
		}
	}
}
