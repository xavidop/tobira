package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPluginsDefaults(t *testing.T) {
	for name, body := range map[string]string{
		"no section":    "connections: []\n",
		"null section":  "plugins:\n",
		"empty section": "plugins: {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := Load(writeTemp(t, body))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := c.PluginSettings(), (Plugins{Enabled: true}); !reflect.DeepEqual(got, want) {
				t.Fatalf("PluginSettings = %+v, want %+v", got, want)
			}
		})
	}
	var nilCfg *Config
	if !nilCfg.PluginSettings().Enabled {
		t.Error("a nil Config must mean plugins enabled")
	}
}

func TestPluginsFull(t *testing.T) {
	c, err := Load(writeTemp(t, "plugins:\n  enabled: false\n  dirs: [~/my-plugins, /opt/tobira/plugins]\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := Plugins{Enabled: false, Dirs: []string{"~/my-plugins", "/opt/tobira/plugins"}}
	if got := c.PluginSettings(); !reflect.DeepEqual(got, want) {
		t.Fatalf("PluginSettings = %+v, want %+v", got, want)
	}
}

func TestPluginsErrors(t *testing.T) {
	for body, want := range map[string]string{
		"plugins: [a]\n":                      "plugins must be a mapping",
		"plugins:\n  enabled: yes please\n":   "plugins.enabled: must be true or false",
		"plugins:\n  dirs: /one\n":            "plugins.dirs: must be a list of directories",
		"plugins:\n  dirs: [\"\"]\n":          "plugins.dirs: entries must not be empty",
		"plugins:\n  dirs: [\"a\\u0007b\"]\n": "plugins.dirs: entries must not contain control characters",
		"plugins:\n  dirs: [[a]]\n":           "plugins.dirs: every entry must be a string",
		"plugins:\n  enable: true\n":          `unknown plugins key (did you mean "enabled"?)`,
		"plugins:\n  hunter2: true\n":         "unknown plugins key",
	} {
		_, err := Load(writeTemp(t, body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", body, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%q: the error quotes the unknown key", body)
		}
	}
}

func TestPluginsDir(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got, want := PluginsDir(env(map[string]string{"XDG_CONFIG_HOME": "/x"}), "/home/me"), filepath.Join("/x", "tobira", "plugins"); got != want {
		t.Errorf("with XDG_CONFIG_HOME: %q, want %q", got, want)
	}
	if got, want := PluginsDir(env(nil), "/home/me"), filepath.Join("/home/me", ".config", "tobira", "plugins"); got != want {
		t.Errorf("with home: %q, want %q", got, want)
	}
	if got := PluginsDir(env(nil), ""); got != "" {
		t.Errorf("with neither: %q, want empty", got)
	}
}

func TestExpandPluginDirs(t *testing.T) {
	got := ExpandPluginDirs([]string{"~/p", "/abs", "~"}, "/home/me")
	want := []string{filepath.Join("/home/me", "p"), "/abs", "/home/me"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExpandPluginDirs = %v, want %v", got, want)
	}
}
