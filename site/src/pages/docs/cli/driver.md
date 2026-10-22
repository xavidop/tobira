---
layout: ../../../layouts/DocsLayout.astro
title: tobira driver
---

# Drivers and plugins

`tobira driver` lists and inspects the drivers tobira can use, built-in ones and external plugins,
and installs plugin binaries. A plugin is a `tobira-driver-<name>` program in the plugins directory
or on `PATH`; see [External plugin](/docs/write-a-driver/plugin/).

```sh
tobira driver list
tobira driver info postgres
tobira driver install https://example.com/tobira-driver-duckdb --sha256 <64 hex characters>
```

## tobira driver

```text
tobira driver list [--no-start]
tobira driver info <name>
tobira driver install <url|path> --sha256 <hex> [--name N] [--force] [--timeout D]
```

### list

Prints a table on a terminal and a JSON array otherwise. Every element has `name`, `kind`,
`version`, `source` (`builtin` or `plugin:<path>`), `status` and `capabilities`.

- To show a plugin's real kind, version and capabilities, `list` starts each plugin (four at a
  time, 5 seconds each). One that cannot answer has status `error: <reason>`, with credentials
  redacted.
- `--no-start` starts nothing: a plugin then shows status `not started`, kind `other` and no
  capabilities.
- A plugin that is not used, because a built-in driver or an earlier directory (the plugins
  directory before `PATH`) has the same name, is listed after the winner with status
  `shadowed: <reason>`, and a `warning: plugin "x" at <path> is not used: <reason>` line goes to
  stderr.
- With `plugins.enabled: false` only built-in drivers are listed, with a note on stderr.

### info

Shows one driver: kind, version, source, status, schemes, dialect, every capability (supported or
not) and the plugins of that name that are not used. For a plugin it also shows the binary's `path`
and `sha256`, and starts the plugin to ask for its details. Text on a terminal, a JSON object
otherwise. An unknown name exits 1.

### install

Puts a plugin binary into the plugins directory: the first `plugins.dirs` entry of the config, else
`$XDG_CONFIG_HOME/tobira/plugins`, created with mode 0700. A directory that is world- or
group-writable, or owned by a user other than you or root, is refused.

- **Source.** A URL when it starts with a scheme and `://`, a local file otherwise. A URL must be
  `http` or `https`. A download follows at most 5 redirects, each to `http` or `https` only, and
  never from `https` back to `http`. Connecting, the TLS handshake and waiting for the response
  headers are each limited to 30 seconds, even with `--timeout 0`. A local symlink is resolved, and
  a note says which file was copied. A directory or other non-regular file is refused.
- **`--sha256` is required**: exactly 64 hex characters, in either case. The binary is kept only
  if its sha256 matches. A mismatch leaves nothing behind.
- **Size.** The binary is capped at 256 MiB, checked while it streams. It is written atomically
  and made 0755.
- **Name.** `--name` (`tobira-driver-<name>` or just `<name>`), or else the URL's or file's base
  name, which must then be `tobira-driver-<name>`. A name must match
  `tobira-driver-[a-z0-9][a-z0-9-]{0,62}`.
- **Existing plugin.** A plugin already installed under that name is refused unless `--force` is
  given.
- **`--timeout`** bounds the download or copy (default 5m; 0 for none).

`install` never runs the binary. It prints the name and the path it wrote; `tobira driver info
<name>` is what starts it.

Exit codes for `install`:

| Code | Meaning |
|---|---|
| `1` | A missing or malformed `--sha256`, a bad name or source, or an existing plugin. |
| `2` | The download failed (network, a refused redirect, a status other than 200). |
| `3` | A checksum mismatch, a binary over the size cap, or a timeout. |
| `4` | An unsafe plugins directory. |
