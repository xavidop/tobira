package host

import (
	"slices"
	"testing"
)

func TestPluginEnvAllowList(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin",
		"HOME=/home/me",
		"TMPDIR=/tmp/x",
		"LANG=en_US.UTF-8",
		"LC_ALL=C",
		"LC_TIME=es_ES",
		"TZ=Europe/Madrid",
		"TOBIRA_PLUGIN_FOO=bar",
		"TOBIRA_PLUGIN_LOG_LEVEL=debug",
		"TOBIRA_PLUGIN_FOO_SECRET=s",
		"TOBIRA_PLUGIN_DB_PASSWORD=s",
		"TOBIRA_PLUGIN_API_TOKEN=s",
		"TOBIRA_PLUGIN_API_KEY=s",
		"tobira_plugin_lower_key=s",
		"TOBIRA_PLUGIN_MAGIC=forged",
		"TOBIRA_CONN_X_PASSWORD=s",
		"TOBIRA_CONFIG=/c",
		"GEMINI_API_KEY=s",
		"FOO_TOKEN=s",
		"AWS_SECRET_ACCESS_KEY=s",
		"PLUGIN_CLIENT_CERT=forged",
		"USERPROFILE=C:\\Users\\me",
		"LCX=1",
		"=C:=C:\\",
		"NOEQUALS",
		"TOBIRA_PLUGIN_NUL=a\x00b",
	}
	got := pluginEnv(environ, "linux")
	want := []string{
		"PATH=/usr/bin",
		"HOME=/home/me",
		"TMPDIR=/tmp/x",
		"LANG=en_US.UTF-8",
		"LC_ALL=C",
		"LC_TIME=es_ES",
		"TZ=Europe/Madrid",
		"TOBIRA_PLUGIN_FOO=bar",
		"TOBIRA_PLUGIN_LOG_LEVEL=debug",
	}
	if !slices.Equal(got, want) {
		t.Errorf("pluginEnv =\n%q\nwant\n%q", got, want)
	}
}

func TestPluginEnvWindows(t *testing.T) {
	environ := []string{
		"Path=C:\\Windows",
		"SystemRoot=C:\\Windows",
		"USERPROFILE=C:\\Users\\me",
		"APPDATA=C:\\Users\\me\\AppData\\Roaming",
		"LOCALAPPDATA=C:\\Users\\me\\AppData\\Local",
		"TEMP=C:\\Temp",
		"TMP=C:\\Temp",
		"PATHEXT=.EXE",
		"tobira_plugin_foo=bar",
		"Tobira_Plugin_Foo_Token=s",
		"GEMINI_API_KEY=s",
	}
	got := pluginEnv(environ, "windows")
	want := []string{
		"Path=C:\\Windows",
		"SystemRoot=C:\\Windows",
		"USERPROFILE=C:\\Users\\me",
		"APPDATA=C:\\Users\\me\\AppData\\Roaming",
		"LOCALAPPDATA=C:\\Users\\me\\AppData\\Local",
		"TEMP=C:\\Temp",
		"TMP=C:\\Temp",
		"PATHEXT=.EXE",
		"tobira_plugin_foo=bar",
	}
	if !slices.Equal(got, want) {
		t.Errorf("pluginEnv =\n%q\nwant\n%q", got, want)
	}
}
