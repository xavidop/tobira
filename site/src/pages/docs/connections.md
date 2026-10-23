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

## SSH tunnels

A database behind a bastion host gets an `ssh:` block. tobira opens an SSH session to that host,
listens on a loopback port and forwards it to the host and port the DSN names, as seen from the SSH
server. The driver connects to the loopback port and knows nothing about SSH.

```yaml
connections:
  - name: prod-pg
    dsn: postgres://app@db.internal:5432/app
    password: env:PROD_PG_PASSWORD
    ssh:
      host: bastion.example.com      # host or host:port; port 22 by default
      user: deploy                   # the SSH login; your user name by default
      key: ~/.ssh/id_ed25519         # a private key file; see "How it authenticates"
      passphrase: env:KEY_PASS       # a secret reference, only for an encrypted key
```

**Which DSNs work.** The DSN must be of the form `scheme://[user[:password]@]host[:port]/...` and
name one host, which is what every network driver takes (PostgreSQL, MySQL, SQL Server, Oracle,
ClickHouse, MongoDB, Redis, Cassandra, Neo4j, Qdrant, Elasticsearch and the other HTTP drivers). A
port can be left out for the common schemes; otherwise add it. A `mongodb+srv://` DSN, a DSN
that lists several hosts (a replica set or a cluster) and a file database such as SQLite cannot be
tunnelled: a replica set or a cluster redirects the client to its members' real addresses, which
the tunnel does not reach. Managed cloud services need no tunnel.

**How it authenticates.** In this order: `key`, if set (with `passphrase` when the file is
encrypted); the SSH agent at `$SSH_AUTH_SOCK`; without a `key`, the default keys
`~/.ssh/id_ed25519`, `~/.ssh/id_ecdsa` and `~/.ssh/id_rsa` that exist; and `password`, a secret
reference, if set. `passphrase` and `password` take the same references as a connection
`password`, never a value. An SSH config file (`~/.ssh/config`) is not read: name the host, port
and user in the block.

**Host keys are checked, never trusted on first use.** The server's key must be in
`~/.ssh/known_hosts` (or the file `known_hosts:` names). If you have connected to the host with
`ssh` before, it is. Otherwise add it once:

```sh
ssh-keyscan -H bastion.example.com >> ~/.ssh/known_hosts
```

An unknown host, a changed key and a missing file are each a clear error that says so. tobira
never writes to `known_hosts`.

**TLS through a tunnel.** The driver sees `127.0.0.1`, so a driver that turns TLS on only for
remote hosts connects in clear inside the tunnel, which SSH encrypts end to end. A DSN that asks
for TLS still gets it, but a mode that checks the certificate against the host name
(`sslmode=verify-full`, say) fails, because the name is now `127.0.0.1`: use `verify-ca`, or the
driver's option for the expected server name. See [Network security](/docs/security/network/).

`tobira doctor` checks the block before any network: the DSN can be forwarded, the key file
exists and `known_hosts` exists; with a ping, it opens the connection through the tunnel.
`tobira connections list` shows `user@host` in an `SSH` column, and the export and import keep the
block. The connection form in the TUI does not edit it: write the block in the file.

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
