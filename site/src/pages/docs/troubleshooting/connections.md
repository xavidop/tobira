---
layout: ../../../layouts/DocsLayout.astro
title: "Troubleshooting: connections and passwords"
---

# Troubleshooting: connections and passwords

Fixes for a connection listed as broken, a password that does not resolve, the OS keyring, file
permissions and connection import. Run `tobira doctor` first: its `config`, `refs` and
`permissions` rows usually point at the cause.

## A connection is listed as broken

One bad entry in the config file does not stop the rest: every command warns
`warning: config <path>: line N: connection "x": <reason>`, `tobira connections list` shows it in
the BROKEN column, and only a command that needs that connection exits 1. The usual reasons are a
literal password (use a reference: `env:`, `file://`, dotenv or `keyring:`) or an unknown key. A
DSN that carries a password is not broken: it works, and `tobira doctor` warns about it. Move the
password to `password:`. Fix the line, or remove the entry with
`tobira connections rm --line N --yes`. See [Managing connections](/docs/connections/).

## A password does not resolve

`tobira doctor` shows each reference as `<scheme> ref resolves` or
`<scheme> ref does not resolve: <reason>`, where the reason is a class such as `not found` or
`permission denied`, never the value or the path.

- **`env:NAME`** is resolved in the environment of the tobira process. An MCP client or an IDE
  usually starts tobira with its own environment, not your shell's: set the variable in the client's
  server config, or use `keyring:`.
- **`file://`** needs an absolute path that the user running tobira can read.
- **`unknown ref scheme`** means no provider in this build handles the scheme. `op://`,
  `aws-sm://`, `vault://` and other secret managers are not supported; use `env:`, `file://`,
  dotenv or `keyring:`.
- **`TOBIRA_CONN_<NAME>_PASSWORD`**, when set, replaces the reference; doctor says so.

## An SSH tunnel does not open

A connection with an `ssh:` block fails before the driver runs when the tunnel cannot open. The
error starts with `ssh tunnel:` and says which step failed; `tobira doctor` checks the files
without any network.

- **`host key is not in known_hosts`** or **`known_hosts file ... does not exist`**: add the
  server once with `ssh-keyscan -H <host> >> ~/.ssh/known_hosts`, or connect to it with `ssh` once.
  tobira never trusts a host on first use.
- **`host key does not match known_hosts`**: the server's key changed, or something sits between
  you and it. Check with the server's owner before removing the old line.
- **`authentication failed`**: no key the server accepts was offered. Set `key:` to the right
  file, or run an SSH agent that holds it (`ssh-add`), or set `password:` to a secret reference.
  `~/.ssh/config` is not read: a host alias, user or identity file set there must go in the block.
- **`key ... is encrypted; set ssh.passphrase`** and **`wrong passphrase`**: the key needs its
  passphrase as a secret reference.
- **`the DSN must be of the form scheme://host:port/...`**, **`names several hosts`** or
  **`has no port`**: the tunnel forwards to exactly one host and port. See
  [which DSNs work](/docs/connections/ssh/#which-dsns-work).
- **The database refuses the certificate** (`verify-full`, host name mismatch): the driver now
  connects to `127.0.0.1`. Use `verify-ca` or the driver's server-name option.

## The OS keyring

`keyring:<name>` reads the password `tobira connections set-password <name>` stored, from the macOS
Keychain, the Windows Credential Manager or the Secret Service on Linux.

- **`keyring ref does not resolve: not found`.** Nothing is stored under that name yet. Run
  `tobira connections set-password <name>`, which stores the password under `<name>` and points the
  connection's `password:` at `keyring:<name>`.
- **`could not store the password ... (is a keyring service running?)`.** On Linux this needs a
  Secret Service provider (GNOME Keyring, KWallet) running and unlocked in your session, which a
  headless server or a container usually does not have. Use `env:` or `file://` there instead.
- **No terminal.** `set-password` reads with no echo on a terminal; in a script, pipe one line and
  pass `--stdin`.

## File permissions

doctor warns when the config file or the history file is group- or world-readable or writable, and
prints the `chmod 600` that fixes it. doctor only reports: it opens the history file read-only and never
changes a mode itself. The `history` row also warns when tobira cannot write the history file or its
directory, in which case queries still run but are not recorded. tobira writes both with mode 0600
itself; a looser mode usually comes from an editor, a copy or a dotfiles checkout.

## Importing connections

- **pgpass refused.** A `~/.pgpass` with any group or world permission bit is refused, as libpq
  does; `chmod 600 ~/.pgpass`.
- **Nothing was written.** Import is a dry run by default; add `--apply`.
- **Passwords were not imported.** Without a terminal, a found password is skipped unless you pass
  `--keyring-yes`; `--no-passwords` skips them always. DBeaver's encrypted credentials and the
  TablePlus keychain are never read, so set those passwords with `tobira connections set-password`.
- **`driver not available in this build`.** The source names a database tobira has no driver for.

See [importing connections](/docs/connections/import/).
