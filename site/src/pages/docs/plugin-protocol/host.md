---
layout: ../../../layouts/DocsLayout.astro
title: "Plugin host"
---

# Plugin host

tobira's side of the protocol is the `plugin/host` package. To the terminal UI, the CLI, the AI
assistant and MCP, a plugin is just another driver. This page says how tobira finds, starts and
supervises plugins.

## Behaviour

- **Discovery.** Executables named `tobira-driver-<name>`, matching
  `^tobira-driver-[a-z0-9][a-z0-9-]{0,62}$` (a trailing `.exe` is ignored on Windows), in the plugins
  directory and then on `PATH`. Symlinks are resolved, and the target must be a regular executable
  file that is not world-writable, in a directory that is not world-writable (unless it is sticky
  and owned by root or you, like `/tmp`). Relative `PATH` entries are skipped. Before each start the
  binary is checked again and must be the same file discovery saw (device, inode, mode and size);
  a swapped binary is refused until tobira restarts. The first one found for a name wins, and a built-in
  driver of the same name wins over any plugin. The `plugins:` section of the
  [configuration](/docs/configuration/#the-plugins-section) turns discovery off or changes the
  directories.
- **Start.** Lazily, and only when something needs the plugin itself: the first `Open` of the
  driver, `tobira driver list` or `driver info`, or `tobira ask` and `tobira mcp` before they build
  their tools (see [what you see before a plugin starts](/docs/write-a-driver/plugin/#limits)). Asking a
  driver for its `Info` never waits and never starts anything: until the plugin has answered, the
  driver shows its name, kind `other`, its name as its only scheme, and no capabilities, so listing
  drivers, validating a connection and drawing the TUI never stall on or launch a plugin. The host
  then caches the plugin's `Info`, with the registry name in place of the plugin's own. A connection
  offers only the capabilities the plugin declared in `Info`, and a transaction only those of its
  connection. A start that hangs is abandoned after 10 seconds, and every caller waiting on it stops
  when its own context ends.
- **Handshake errors.** A binary that wants another host's magic cookie, and a plugin built for
  another protocol version, both fail with a connection error that says which.
- **Environment.** Only `PATH`, `HOME`, `TMPDIR`, `LANG`, `LC_*`, `TZ` and `TOBIRA_PLUGIN_*` (minus
  names ending in `_PASSWORD`, `_TOKEN`, `_KEY` or `_SECRET`) are passed, plus go-plugin's own
  variables. On Windows, `SystemRoot`, `USERPROFILE`, `APPDATA`, `LOCALAPPDATA`, `TEMP`, `TMP` and
  `PATHEXT` are added so the process can run at all. Secrets travel only in `Open`'s `ConnConfig`.
- **Input and output.** Stdin is the null device, never tobira's own. Stdout is never shown. Each stderr line goes to the debug log at debug level, redacted
  with every config the process was given, with control characters escaped and lines capped at
  4 KiB.
- **Crashes.** If the plugin process dies, calls in flight and every later call on its connections
  fail with `connection failed`. The next `Open` starts a new process; connections opened on the
  dead one stay dead.
- **Cancellation.** Every call honours its context, and cancelling a stream cancels the RPC.
  Calls without a context (`Close`, `Invalidate`) are bounded at 5 seconds. On exit tobira asks each
  plugin to shut down, then kills it. If tobira itself is killed, SDK plugins exit on their own, and
  on Linux every plugin gets `SIGKILL` as its parent-death signal.
- **Untrusted answers.** Everything a plugin returns is decoded under the [limits](/docs/plugin-protocol/wire/#limits). An
  unknown capability string is ignored, and error messages are redacted again with the
  connection's secrets.
- **Vector search.** A text `Similar` query is embedded by the host with the configured embedder,
  and the vector is sent along with the text.
