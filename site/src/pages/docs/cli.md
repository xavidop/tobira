---
layout: ../../layouts/DocsLayout.astro
title: CLI reference
---

# CLI reference

Every tobira feature is a `tobira` subcommand. Run `tobira` with no arguments in a terminal to open
the [terminal UI](/docs/tui/); when stdin or stdout is not a terminal, it prints help instead. This
page lists the commands and the rules they share: global flags, exit codes and how errors are
printed.

## Quick start

```sh
tobira query sqlite:./demo.db "select * from pets"     # run one query
tobira connect demo                                      # open the terminal UI on a connection
tobira connections list                                  # list configured connections
tobira doctor                                            # check the setup
tobira --help                                            # every command
```

## Commands

| Command | What it does |
|---|---|
| [`tobira query`](/docs/cli/query/) | Run one query and print the result, or write it to a file |
| [`tobira connect`](/docs/cli/connect/) | Open the terminal UI with one connection expanded |
| [`tobira connections`](/docs/cli/connections/) | List, add, edit, remove, test, import and export connections, and store passwords in the OS keyring |
| [`tobira ask`](/docs/cli/ask/) | Ask the AI assistant a question about a database |
| [`tobira mcp`](/docs/cli/mcp/) | Serve chosen connections to an MCP client |
| [`tobira history`](/docs/cli/history/) | List the queries you and your tools ran |
| [`tobira driver`](/docs/cli/driver/) | List and inspect drivers, and install a plugin (with a required `--sha256`) |
| [`tobira doctor`](/docs/cli/doctor/) | Check the config, connections, secrets, plugins, AI and MCP setup |
| [`tobira watch`](/docs/cli/watch/) | Stream a connection's change events |
| [`tobira version`](#tobira-version) | Print the version |

`tobira completion` prints a shell completion script, and `tobira help` shows help for any
command.

## Global flags

| Flag | Meaning |
|---|---|
| `--config <path>` | Config file path. See [Configuration](/docs/configuration/#where-the-file-lives). |
| `--debug` | Write JSON debug logs to `$XDG_STATE_HOME/tobira/tobira.log` (`~/.local/state/tobira/tobira.log` when `XDG_STATE_HOME` is unset), never to stdout or stderr. |

`tobira --read-only` opens the terminal UI with every connection of the session read-only. It has
no effect when help is printed.

## tobira version

```text
tobira version
```

Prints the version, commit and build date on one line, such as
`tobira 1.0.0 (abc1234, 2026-10-19)`.

## Exit codes

Every command uses the same codes, except [`tobira doctor`](/docs/cli/doctor/), which has its own.

| Code | Meaning |
|---|---|
| `0` | Success. |
| `1` | Usage error: bad flags (including a negative `--limit` or `--timeout`), a missing query, an unknown connection name, a bad config file, a command that needs a terminal run without one, an invalid or duplicate connection. |
| `2` | Could not open the connection: an unreachable server, rejected credentials, an invalid DSN, a missing database or file, no driver for the DSN, or a secret that could not be resolved. |
| `3` | Any other query error: syntax, not found, timeout, cancellation (also while connecting), a driver that cannot explain. |
| `4` | Read-only violation or permission denied, or refused by policy (`tobira ask --send-rows` under `ai.send_rows: never`). |

## Errors and redaction

Errors go to stderr as one `error: <message>` line. tobira always redacts DSN passwords, the
resolved password, the configured `password:` setting and any `password=`, `password: ...` or
`"password": "..."` value in what it prints. A bare password shorter than 4 bytes cannot be told
apart from ordinary text, so it is masked only where it is named. Control characters are shown as
escapes (`\x1b`). The `table` format escapes control characters in cells the same way; `json`,
`ndjson` and `csv` print values unchanged.

## Broken config entries

One bad connection entry does not stop the others. Every command prints a line to stderr for each
entry that did not load, and carries on without it:

```text
warning: config /home/me/.config/tobira/config.yaml: line 7: connection "pg": password must be a secret reference
```

A command that needs the broken entry (`tobira query pg ...`, `tobira connect pg`) exits 1 with
`connection "pg": <what is wrong>`. The message never contains the value that failed. A config file
that is not valid YAML, or has a bad `tui:` section, is still a fatal exit 1, except for
`tobira connect <dsn>`. See [Managing connections](/docs/connections/#broken-entries).
