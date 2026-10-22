---
layout: ../../layouts/DocsLayout.astro
title: Troubleshooting
---

# Troubleshooting

When something does not work, run `tobira doctor` first. It checks your config, connections,
secrets, plugins, AI and MCP setup in one go, and its first `fail` row usually explains the rest.
The pages below cover the common problems by area.

## Quick start

```sh
tobira doctor            # every check, with read-only pings of every connection
tobira doctor --no-ping  # no network at all: no pings, no remote secret refs
tobira doctor --online   # also ask the AI provider and start each plugin
```

Each row is `ok`, `skip`, `warn` or `fail`, with a detail that never contains a secret. doctor
prints a table on a terminal and JSON otherwise, and exits 0 when everything is ok (skips
included), 1 on a warning and 2 on a failure, so it fits in a script. Every flag is in the
[`tobira doctor` reference](/docs/cli/doctor/).

Read the first `fail` row first: a broken config entry or an unresolvable password usually explains
the connection failures below it.

## By area

| Page | Covers |
|---|---|
| [Connections and passwords](/docs/troubleshooting/connections/) | Broken config entries, passwords that do not resolve, the OS keyring, file permissions, connection import |
| [Read-only refusals](/docs/troubleshooting/read-only/) | Why a statement was refused with exit code 4, per driver |
| [Driver connections](/docs/troubleshooting/drivers/) | DSN and TLS problems for each database, and emulators |
| [AI and MCP](/docs/troubleshooting/ai-and-mcp/) | API keys, models, providers, and MCP clients that see nothing or get 401 or 403 |
| [Plugins](/docs/troubleshooting/plugins/) | Plugins that are not found, refused or failing |

## Exit codes

Every command uses the same codes, so the code alone tells you where to look:

| Code | Meaning | Look at |
|---|---|---|
| 0 | ok | |
| 1 | usage or config error | the flags, or the `config` rows of `tobira doctor` |
| 2 | connection error (also auth, unknown driver) | the `connections` and `refs` rows, and [driver connections](/docs/troubleshooting/drivers/) |
| 3 | query error | the statement and the driver's message |
| 4 | refused by policy (read-only, permission) | [read-only refusals](/docs/troubleshooting/read-only/) |

`tobira doctor` itself uses 0, 1 and 2 for ok, warnings and failures.

## Debug log

Run any command with `--debug` for more detail. The log goes to
`$XDG_STATE_HOME/tobira/tobira.log` (default `~/.local/state/tobira/tobira.log`), never to the
terminal, and it never contains passwords.

## Still stuck

Open an [issue](https://github.com/xavidop/tobira/issues) with the `--debug` log of the failing
command and the `tobira doctor` output. Report anything that looks like a security problem
privately; see [Security](/docs/security/#reporting-a-vulnerability).
