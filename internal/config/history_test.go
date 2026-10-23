package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestHistorySection(t *testing.T) {
	c, err := Load(filepath.Join("testdata", "brief.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.History == nil || c.History.Path != "~/.local/state/tobira/history.db" {
		t.Fatalf("History = %+v, want the documented path", c.History)
	}
	if len(c.Warnings) != 0 {
		t.Errorf("the documented example warns: %v", c.Warnings)
	}
	for name, body := range map[string]string{
		"no section":   "connections: []\n",
		"null section": "history:\n",
		"empty":        "history: {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := Load(writeTemp(t, body))
			if err != nil {
				t.Fatal(err)
			}
			if c.HistoryPath() != "" {
				t.Errorf("HistoryPath = %q, want empty", c.HistoryPath())
			}
		})
	}
	for _, p := range []string{"/var/tobira/h.db", "~/h.db", "$HOME/h.db", "${XDG_DATA_HOME}/tobira/h.db"} {
		c, err := Load(writeTemp(t, "history:\n  path: \""+p+"\"\n"))
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if c.HistoryPath() != p {
			t.Errorf("HistoryPath = %q, want %q", c.HistoryPath(), p)
		}
	}
	var nilCfg *Config
	if nilCfg.HistoryPath() != "" {
		t.Error("a nil Config has no history.path")
	}
}

func TestHistoryErrors(t *testing.T) {
	for name, tt := range map[string]struct{ body, want string }{
		"not a mapping": {"history: x\n", "history must be a mapping"},
		"unknown key":   {"history:\n  pth: /x\n", `did you mean "path"`},
		"not a string":  {"history:\n  path: [a]\n", "history.path: must be a string"},
		"empty":         {"history:\n  path: \"  \"\n", "history.path: must not be empty"},
		"relative":      {"history:\n  path: state/h.db\n", "history.path: must be absolute"},
		"control":       {"history:\n  path: \"/x\\u0085y\"\n", "history.path: must not contain control characters"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeTemp(t, tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestUnknownTopLevelKeyWarns(t *testing.T) {
	c, err := Load(writeTemp(t, "connections: []\nhistroy:\n  path: /x\nzzz_custom: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(c.Warnings, "\n")
	if !strings.Contains(got, `line 2: unknown top-level key (did you mean "history"?)`) ||
		!strings.Contains(got, "line 4: unknown top-level key; it is ignored") {
		t.Errorf("warnings = %q", got)
	}
	if strings.Contains(got, "zzz_custom") {
		t.Error("a warning quotes the unknown key")
	}
}

func TestHistoryMaxEntries(t *testing.T) {
	for body, want := range map[string]int{
		"history:\n  max_entries: 500\n":                      500,
		"history:\n  path: /x/h.db\n  max_entries: 1000000\n": 1000000,
		"history:\n  max_entries: 1\n":                        1,
		"history:\n  path: /x/h.db\n":                         0,
		"history:\n  max_entries:\n":                          0,
	} {
		c, err := Load(writeTemp(t, body))
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		if c.HistoryMaxEntries() != want {
			t.Errorf("%q: HistoryMaxEntries = %d, want %d", body, c.HistoryMaxEntries(), want)
		}
	}
	var nilCfg *Config
	if nilCfg.HistoryMaxEntries() != 0 {
		t.Error("a nil Config has no history.max_entries")
	}
	for name, tt := range map[string]struct{ body, want string }{
		"not an integer": {"history:\n  max_entries: many\n", "history.max_entries: must be an integer"},
		"a float":        {"history:\n  max_entries: 1.5\n", "history.max_entries: must be an integer"},
		"zero":           {"history:\n  max_entries: 0\n", "history.max_entries: must be from 1 to 1000000"},
		"negative":       {"history:\n  max_entries: -1\n", "history.max_entries: must be from 1 to 1000000"},
		"too large":      {"history:\n  max_entries: 1000001\n", "history.max_entries: must be from 1 to 1000000"},
		"misspelt":       {"history:\n  max_entrie: 5\n", `did you mean "max_entries"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeTemp(t, tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}
