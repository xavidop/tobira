package host

import (
	"slices"
	"strings"

	"github.com/xavidop/tobira/plugin/shared"
)

// pluginEnvPrefix is the prefix of the variables meant for plugins.
const pluginEnvPrefix = "TOBIRA_PLUGIN_"

// allowedKeys are passed to a plugin as they are.
var allowedKeys = []string{"PATH", "HOME", "TMPDIR", "LANG", "TZ"}

// windowsKeys are what a Windows process needs to run at all (system DLLs,
// the temp and profile directories, executable lookup).
var windowsKeys = []string{"SYSTEMROOT", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "TEMP", "TMP", "PATHEXT"}

// secretSuffixes mark a TOBIRA_PLUGIN_ variable that looks like a secret:
// secrets reach a plugin only through Open's ConnConfig.
var secretSuffixes = []string{"_PASSWORD", "_TOKEN", "_KEY", "_SECRET"}

// pluginEnv filters environ (KEY=VALUE entries) down to the plugin env
// allow-list: PATH, HOME, TMPDIR, LANG, LC_*, TZ and TOBIRA_PLUGIN_*, minus
// TOBIRA_PLUGIN_* variables that look like secrets and the magic cookie
// (go-plugin sets that itself), plus on Windows the few variables a process
// needs to start. Keys match case-insensitively on Windows only. go-plugin
// appends its own variables (the cookie, PLUGIN_*) after these.
func pluginEnv(environ []string, goos string) []string {
	var out []string
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" || strings.ContainsRune(kv, 0) {
			continue
		}
		key := k
		if goos == "windows" {
			key = strings.ToUpper(k)
		}
		if allowedKey(key, goos) {
			out = append(out, kv)
		}
	}
	return out
}

func allowedKey(key, goos string) bool {
	switch {
	case slices.Contains(allowedKeys, key), strings.HasPrefix(key, "LC_"):
		return true
	case goos == "windows" && slices.Contains(windowsKeys, key):
		return true
	case strings.HasPrefix(key, pluginEnvPrefix):
		if strings.EqualFold(key, shared.MagicCookieKey) {
			return false
		}
		upper := strings.ToUpper(key)
		return !slices.ContainsFunc(secretSuffixes, func(s string) bool { return strings.HasSuffix(upper, s) })
	}
	return false
}
