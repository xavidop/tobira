// Package host runs external driver plugins: tobira-driver-<name>
// binaries that speak the tobira.plugin.v1 protocol through
// hashicorp/go-plugin (see plugin/sdk for the plugin side).
//
// Discover finds plugin binaries in the plugins directory and on PATH
// (SearchDirs orders them; the plugins directory wins on a duplicate
// name). Register adds a driver.Driver adapter for each to a
// driver.Registry, so the rest of tobira cannot tell a plugin from an
// in-process driver. A built-in driver of the same name always wins; the
// clash is reported by Host.Conflicts.
//
// Plugins start lazily, only on the first Open or a Host.Info call, and are
// reused; a driver's Info never blocks or starts one (it returns a stub
// until the plugin has answered). A binary must not be
// replaceable by others and must be the file discovery saw. Each runs with
// a null stdin and an environment allow-list (PATH, HOME, TMPDIR, LANG, LC_*, TZ
// and TOBIRA_PLUGIN_* minus secret-looking names): secrets reach a plugin
// only in Open's ConnConfig. Its stdout belongs to the go-plugin handshake
// and is never shown; its stderr goes to the host's logger at debug level,
// redacted and capped. Every call honours its context. If a plugin process
// dies, calls on it fail with driver.ErrConnection, its Conns stay dead,
// and the next Open starts a new process. Everything a plugin returns is
// decoded under plugin/convert's caps, and its errors are redacted again
// with the connection's secrets. Host.Close stops every process.
package host
