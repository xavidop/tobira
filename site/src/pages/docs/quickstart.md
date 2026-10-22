---
layout: ../../layouts/DocsLayout.astro
title: Quick start
---

# Quick start

This page walks through `tobira query`, the terminal UI, the query editor, editing rows, export and
`tobira connections` on a SQLite file. You need a `tobira` binary (see
[Install](/docs/install/)); nothing else, because the SQLite driver is pure Go.

## Run a query

Create a small database and query it. The first command writes, so it is not read-only:

```sh
tobira query sqlite:./demo.db "create table pets (id integer primary key, name text, note text)"
tobira query sqlite:./demo.db "insert into pets (name, note) values ('Mochi', null), ('Tama', '')"
tobira query sqlite:./demo.db "select * from pets"
```

On a terminal the result is a table; piped, it is JSON. Ask for a format explicitly with
`--format table|json|ndjson|csv`:

```sh
tobira query sqlite:./demo.db "select * from pets" --format csv
```

```text
id,name,note
1,Mochi,
2,Tama,""
```

Note the last column: Mochi's note is `NULL` (an empty unquoted field) and Tama's is an empty string
(`""`). Every format keeps the two apart; see [the CLI reference](/docs/cli/query/#null-and-empty-strings).

## Stay read-only

`--read-only` refuses anything that would write, and exits with code 4:

```sh
tobira query sqlite:./demo.db "delete from pets" --read-only
# error: sqlite: query: only SELECT, WITH, VALUES, EXPLAIN and read-only PRAGMA statements are allowed on a read-only connection: read-only violation
echo $?   # 4
```

The guard is not text matching alone: SQLite opens the file read-only and `query_only` is on. See
[SQLite](/docs/drivers/sqlite/) for the exact layers.

## Name your connections

A DSN on the command line is fine for a quick look. For anything you use twice, put it in the config
file, `~/.config/tobira/config.yaml`:

```yaml
connections:
  - name: demo
    dsn: sqlite:./demo.db
  - name: local-pg
    dsn: postgres://app@localhost:5432/app?sslmode=disable
    password: env:LOCAL_PG_PASSWORD
    read_only: true
```

Then use the name instead of the DSN:

```sh
tobira query demo "select count(*) as pets from pets"
```

A relative SQLite path is resolved from the directory you run tobira in, so use an absolute path
(`sqlite:/home/me/demo.db`) for a connection you open from anywhere.

A connection with `read_only: true`, like `local-pg` here, is always read-only, whatever flags you
pass. Passwords come from
the environment (`env:NAME`), a file (`file://`), a dotenv file (`dotenv://.env#KEY`) or the OS
keyring (`keyring:NAME`, set with `tobira connections set-password`) rather than from the config
text; see [Passwords and secrets](/docs/secrets/).

## Open the terminal UI

In a terminal, `tobira` opens the browser with every configured connection in the sidebar, and
`tobira connect` opens it with one connection expanded (a name, or a DSN for this session only):

```sh
tobira
tobira connect demo
tobira connect sqlite:./demo.db
```

Move with the arrows, `enter` opens a collection, `f` filters rows (`name ~ 'Mo'`), `s` sorts by the
current column, `y` copies a cell and `q` quits. `F1` shows every key. See the
[terminal UI guide](/docs/tui/).

## Write queries and edit rows

In the terminal UI, `ctrl+e` opens the [query editor](/docs/editor/) below the grid. Type a
statement and press `ctrl+enter` (or `F5`) to run it; the result opens in a tab. On the grid, `e`
edits the current cell, `i` inserts a row and `d` marks one for deletion; `:apply` writes the
changes, and `:tx begin` puts runs and edits inside a transaction ([editing rows](/docs/editing/)).
`:export csv ~/pets.csv` writes the open collection to a file.

From the command line, `--output` does the same for any query, and `tobira history` lists what you
ran:

```sh
tobira query demo "select * from pets" --output pets.csv
tobira history --limit 5
```

See [Export](/docs/export/) and [Query history](/docs/history/).

## Ask the AI assistant

With an `ai:` section in the config file, you can ask questions in plain language, from the terminal
UI (press `a`) or the shell. Nothing is written, and row data is not sent to the model unless you
agree:

```yaml
ai:
  provider: googleai
  api_key: env:GEMINI_API_KEY
```

```sh
tobira ask demo "how many pets have a note?" --collection pets
tobira ask demo "how many pets have a note?" --answer
```

The first proposes a query and runs nothing; the second lets the assistant run read-only queries and
answer. See [AI assistant](/docs/ai/).

## Add a connection

```sh
tobira connections add --name demo2 --driver sqlite --dsn sqlite:./demo.db --read-only
tobira connections list
tobira connections test demo2
```

Without flags in a terminal, `tobira connections add` opens a form. Passwords are always references
such as `env:NAME`; see [Managing connections](/docs/connections/).

## Explain a query

`--explain` prints the query plan instead of running the query, on drivers that can explain:

```sh
tobira query demo "select * from pets where id = 1" --explain
```

## Next

- [Terminal UI](/docs/tui/), the [query editor](/docs/editor/), [editing rows](/docs/editing/) and
  [keymaps](/docs/keymaps/).
- [Managing connections](/docs/connections/) and the [AI assistant](/docs/ai/).
- [CLI reference](/docs/cli/): every flag, format and exit code.
- [Configuration](/docs/configuration/): config file, secrets and password precedence.
- [Drivers](/docs/drivers/): what each database supports.
