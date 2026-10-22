---
layout: ../../layouts/DocsLayout.astro
title: Managing connections
---

# Managing connections

Connections live in the [config file](/docs/configuration/). You can edit the YAML by hand, or add,
edit and remove connections from the [TUI](/docs/tui/) or with `tobira connections`. Both ways
write the same file: comments, key order and every other section are kept, and the write is atomic
(a temporary file in the same directory, then a rename). The file is created with mode 0600 and its
directory with 0700.

If the file changed on disk after tobira read it, the write is refused rather than overwriting the
other change. In the CLI that is `config changed on disk; re-run`.

## Passwords are references, never values

tobira does not store passwords in the config file. The `password:` field takes a reference
(`env:NAME`, `file://`, `dotenv://` or `keyring:NAME`) that is resolved when the connection opens.
A literal password there is a load error, and the value is never echoed. The form and
`tobira connections add` also refuse a DSN that contains a password
(`postgres://app:s3cret@host/db`) and tell you to move it into a reference. A password written
into a DSN by hand still works, but it is masked in every output and `tobira doctor` warns about
it.

[Passwords and secrets](/docs/secrets/) has every scheme, the precedence, and
`tobira connections set-password`, which keeps a password in the OS keyring.

## In the TUI

- `n` (sidebar) or `:connection add` opens the form.
- `E` on a connection, or `:connection edit <name>`, edits it. Renaming works, and the connection
  keeps its `password:` reference. To move a `keyring:` password to the new name, see
  [the OS keyring](/docs/secrets/#the-os-keyring).
- `D` on a connection, or `:connection rm <name>`, deletes it after you type its name (for an entry
  with no name, the label shown in the sidebar).

If the config file changed on disk after tobira read it, the form and the delete prompt are refused
with `config file changed on disk; reload with :reload`; `:reload` re-reads the file.

The form has these fields:

| Field | Rule |
|---|---|
| Name | Required, unique, letters, digits, `.`, `_` and `-` |
| Driver | One of the drivers compiled into the binary |
| DSN | Required; its scheme must belong to the chosen driver; no password |
| Password ref | Optional; `env:`, `file://`, `dotenv://` or `keyring:` |
| Read-only | A toggle |
| Colour | One of `red`, `green`, `yellow`, `orange`, `cyan`, `blue`, `magenta`, or none |
| Tags | Comma-separated |

`ctrl+t` tests the connection (resolve the reference, open, ping) and shows OK or the redacted
error; testing never saves. `ctrl+s` saves and `esc` cancels. After saving, the sidebar updates
without a restart. Editing a connection that is open closes it, and it reopens when you expand it
again.

## On the command line

```sh
tobira connections list
tobira connections add            # the same form, in a terminal
tobira connections add --name local-pg --driver postgres \
  --dsn 'postgres://app@localhost:5432/app?sslmode=require' \
  --password-ref env:LOCAL_PG_PASSWORD --read-only --color green --tag prod --tag eu
tobira connections edit local-pg  # the form, pre-filled; needs a terminal
tobira connections test local-pg
tobira connections rm local-pg --yes
```

`add` with flags is non-interactive, for scripts. `edit` and `rm` also take `--line N` to reach an
entry with no usable name or a duplicated one. [`tobira connections`](/docs/cli/connections/) has
every subcommand, flag and exit code.

## Importing connections

`tobira connections export` prints your connections without passwords, and
`tobira connections import` reads them back or reads DBeaver, pgpass or TablePlus. See
[Importing connections](/docs/connections/import/).

## Broken entries

One entry that cannot be loaded does not take the others down. The entry is called broken when its
name is a duplicate or missing, a key is unknown, a field has the wrong type, or a password,
driver, colour or DSN fails validation. tobira:

- prints `warning: config <path>: line N: connection "x": <reason>` on stderr for every command;
- lists the entry anyway: `tobira connections list` adds a `BROKEN` column (in JSON the
  same object shape as a working entry, with `broken` and `line` set and the other keys empty), and the TUI sidebar shows it after the working
  connections in the failed colour. It cannot be expanded; selecting it shows
  `connection x is misconfigured:` with the reason and the keys to edit or delete it;
- fails only the commands that use it, with exit 1 and `connection "x": <reason>`.

The reason never contains the offending value; an unknown key is never quoted either, only a
"did you mean" hint for a near miss. To fix an entry, edit the file by hand, or use `E` / `D` in the
TUI or `tobira connections edit|rm` (the form is prefilled with the fields that parsed safely).
An entry with no usable name or a duplicated name is reached with `--line N` on the CLI; the TUI
resolves the row to its line itself. A file that is not valid YAML is not an entry-level problem and
still fails the command.
