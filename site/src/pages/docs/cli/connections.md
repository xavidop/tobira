---
layout: ../../../layouts/DocsLayout.astro
title: tobira connections
---

# tobira connections

`tobira connections` manages the connections in the config file. The full guide, with the
password rules and the TUI form, is [Managing connections](/docs/connections/). To open the
terminal UI on one connection, see [`tobira connect`](/docs/cli/connect/).

```sh
tobira connections add --name local-pg --driver postgres --dsn 'postgres://app@localhost/app' --password-ref env:PG_PASSWORD
tobira connections test local-pg
tobira connections set-password local-pg
tobira connections import --from pgpass --apply
```

## tobira connections

```text
tobira connections list
tobira connections add [--name N --driver D --dsn DSN [--password-ref REF] [--read-only] [--color C] [--tag T]...]
tobira connections edit <name> [--line N]
tobira connections rm <name> [--line N] [--yes]
tobira connections test <name>
tobira connections set-password <name> [--no-config] [--stdin] [--force]
tobira connections export [--format yaml|json] [names...]
tobira connections import --from dbeaver|pgpass|tableplus|file [path] [--apply] [--rename-suffix S] [--no-passwords] [--keyring-yes]
```

### list

Prints a table on a terminal and a JSON array otherwise. Every JSON element has the same keys:
`name`, `driver`, `dsn` (password masked), `read_only`, `tags`, `color`, `password_ref`, `line` and
`broken`. A key that does not apply is empty or zero. An entry that did not load has a non-empty
`broken` (its error) and its `line`, and a `BROKEN` column in the table. Names are masked like a
DSN, so a DSN typed into the name field never shows its password.

### add, edit and rm

- `add` is non-interactive when flags are given, and opens a form otherwise (terminal required).
  `--password-ref` takes `env:NAME`, `file:///path`, `dotenv://path#KEY` or `keyring:NAME`.
- `edit` is interactive only.
- `rm` asks you to type the name unless `--yes` is given.
- `edit` and `rm` take `--line N` instead of a name, to address an entry by the line it starts on.
  That is the only way to reach an entry with no usable name, and it is required when a name
  appears more than once.

### test

Prints `ok (<duration>)` and exits 0, or exits 2 when the connection cannot be opened.

### set-password

Stores a password in the OS keyring and points the connection at it with `keyring:<name>`.
`--stdin` reads one line of stdin instead of prompting, `--no-config` leaves the config file
alone, and `--force` stores even when another connection uses the same entry. See
[the OS keyring](/docs/secrets/#the-os-keyring).

### export

Prints the named connections (all by default) to stdout in the config's `connections:` shape, as
YAML or `--format json`. Password references are kept. A literal password, a password inside a DSN
and an option that looks like a secret are left out, with a note on stderr. An unknown name exits 1.

### import

Reads DBeaver's `data-sources.json`, a libpq pgpass file (default `$PGPASSFILE` or `~/.pgpass`),
TablePlus's `Connections.plist` (default: its macOS location) or a file `export` wrote. It is a dry
run until `--apply`. `--rename-suffix` (default `-imported`) renames a taken name, `--keyring-yes`
stores found passwords in the OS keyring without asking, and `--no-passwords` skips them. See
[Importing connections](/docs/connections/import/).

### Exit codes

A duplicate or invalid connection, a config file that changed on disk, and a missing terminal where
one is needed all exit 1. The other codes are the [shared ones](/docs/cli/#exit-codes).
