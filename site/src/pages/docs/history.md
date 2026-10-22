---
layout: ../../layouts/DocsLayout.astro
title: Query history
---

# Query history

tobira keeps a local log of the queries it runs, so you can find that statement from last week with
`tobira history`, and walk back through it with `↑` and `ctrl+r` in the
[query editor](/docs/editor/#history). Recall offers only what you ran (`tobira query` and the
editor): the tool calls of the AI assistant and of MCP clients are listed by `tobira history` but
never recalled.

## Where it lives

A SQLite file at `$XDG_STATE_HOME/tobira/history.db` (`~/.local/state/tobira/history.db` when
`XDG_STATE_HOME` is unset, on every OS), or wherever the
[`history.path`](/docs/configuration/#the-history-section) config key points. The directory is
created with mode 0700 and the file with 0600. It keeps the last 10,000 entries: recording a new one removes the oldest beyond that.

History is best effort. If the file cannot be opened or written, `tobira query` and the editor
behave exactly as without it; in the editor, `↑` and `ctrl+r` are then off. Listing never creates
the file: with no history yet, `tobira history` prints nothing (`[]` for `json`) and exits 0.

## What is recorded

| Source | Recorded |
|---|---|
| `cli` | Every `tobira query` run: ordinary runs, `--explain` runs, `--output` exports (`rows` is `-1` for a PostgreSQL native csv export, where the count is unknown) and runs the read-only guard refused or that failed (`ok` false). |
| `tui` | Every editor run, `:explain` included, whether it succeeded, failed, was refused by the read-only guard or was cancelled, with `ctrl+c` or by quitting while it ran (recorded with `ok` false before tobira exits). |
| `mcp` | Every call an MCP client made, with the client's name. |
| `ai` | Every `query`, `sample`, `explain` and `execute` the [AI assistant](/docs/ai/) ran, with the same credential masking. The question you asked and the model's text are not recorded. |

Browsing, completion lookups, inline edits written by `:apply` and `:export` are not recorded: the
log is of the queries you ran, not of every statement tobira sent. An ad-hoc DSN is recorded under
its redacted form.

## Credentials are masked

The query text is stored after the same redaction errors go through: DSN passwords, the connection's
resolved password and the values in `password=`, `password: x` and `"password": "x"` forms are
replaced by `xxxxx` before anything is written. Other secret-named keys (`token`, `secret`, `sig`,
`sas`, `credentials`, `api_key`, `key` and their variants) are masked only in the `key=value` form
of a DSN, URL query or connection string; in a JSON query such as
`{"filter":{"token":"t-123"}}` they are data and stored as typed. A resolved password shorter than
4 bytes cannot be told apart from ordinary text, so it is masked only where it is named
(`password=...`).

Masking replaces every literal equal to the resolved password (4 bytes or more), wherever it
appears, so a query that merely contains the same text loses it too. With the password `test`,
`UPDATE t SET s = 'test'` is stored, listed and recalled with `↑`/`ctrl+r` as
`UPDATE t SET s = 'xxxxx'`. The recalled text is what was stored: check it before you run it again,
or it writes `xxxxx`.

This is masking, not scrubbing: a secret you type into a query's own text in some other shape (a
literal in an `INSERT`, say) is stored as typed. Treat the file like a shell history.

## Listing it

```sh
tobira history
tobira history --conn local-pg --search "from orders" --limit 20
tobira history --source ai
tobira history --mcp
tobira history --format csv > history.csv
```

Flags are in the [CLI reference](/docs/cli/history/). Each entry has these fields:

| Field | Meaning |
|---|---|
| `at` | When the run started, RFC 3339 in UTC |
| `conn` | The connection name, or the redacted DSN for an ad-hoc one |
| `source` | `cli`, `tui`, `ai` or `mcp` |
| `client` | The MCP client's name for an `mcp` entry (escaped, at most 64 characters), else null. Shown in `json` and `ndjson` always, and in `table` and `csv` when the listing is `--mcp` or has an `mcp` entry |
| `ok` | Whether the run succeeded |
| `rows` | Rows returned, or rows affected for a statement with no result set |
| `ms` | How long it took, in milliseconds |
| `query` | The masked statement |

With `--format json` a listing is one `{"columns", "rows", "stats"}` document like `tobira query`
prints; with no entries it is `[]`.
