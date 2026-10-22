---
layout: ../../../layouts/DocsLayout.astro
title: "Security: plugin trust"
---

# Security: plugin trust

An external driver plugin (`tobira-driver-<name>`) is a program you run as yourself. tobira does
not sandbox it, so a plugin can read your files and use your network. Install only binaries you
trust, from a source you can check. This page lists what tobira does to limit the damage.

## Quick start

```sh
sha256sum tobira-driver-example            # macOS: shasum -a 256; compare with the author's checksum
tobira driver install ./tobira-driver-example --sha256 <64 hex characters>
tobira driver info example                 # starts it for the first time
```

`plugins.enabled: false` in the [config file](/docs/configuration/#the-plugins-section) turns
plugin discovery off.

## What tobira does

- **Install** requires `--sha256` and keeps the binary only if it matches. It caps the download at
  256 MiB, writes into a 0700 plugins directory, replaces an existing plugin only with `--force`
  and never runs the binary.
- **Discovery** accepts only `tobira-driver-<name>` files with a safe name, in the plugins
  directory and on `PATH` (never a relative entry). A world-writable binary, or one in a
  world-writable directory that is not sticky, is refused. Before every start the file must still
  be the one discovery found. `tobira doctor` flags a plugins directory that is group- or
  world-writable.
- **Environment.** A plugin gets only `PATH`, `HOME`, `TMPDIR`, `LANG`, `LC_*`, `TZ` and
  `TOBIRA_PLUGIN_*` (minus names ending in `_PASSWORD`, `_TOKEN`, `_KEY` or `_SECRET`). The
  `TOBIRA_CONN_*` variables and your AI provider keys are never passed.
- **Secrets.** A connection's password reaches the plugin only when that connection opens, over a
  local gRPC connection secured with mutual TLS.
- **Streams.** stdin is `/dev/null`, so a plugin cannot read your keystrokes or an MCP stream.
  stdout is reserved for the handshake, and stderr goes to the `--debug` log, redacted and escaped.
- **Answers** are checked against size and depth limits before use, and error text is redacted
  again by tobira.

## What tobira cannot do

- A plugin's read-only guard is its own promise, which tobira cannot verify.
- A checksum proves the file is the one you meant, not that it is safe.
- On Windows a plugin can outlive a tobira process that was killed.

See [External plugin](/docs/write-a-driver/plugin/) and the [plugin host](/docs/plugin-protocol/host/).
