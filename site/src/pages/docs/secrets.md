---
layout: ../../layouts/DocsLayout.astro
title: Passwords and secrets
---

# Passwords and secrets

tobira never stores a password in its config file. A connection's `password:` is a reference that
is resolved when the connection opens, and the resolved password is held as a secret that is never
printed or logged.

## Where the password comes from

For connection `<name>`, the password comes from the first of these that is set:

1. The environment variable `TOBIRA_CONN_<NAME>_PASSWORD`. `<NAME>` is the connection name in upper
   case with every non-alphanumeric character turned into `_`, so `local-pg` reads
   `TOBIRA_CONN_LOCAL_PG_PASSWORD`.
2. The `password:` reference in the config file.
3. The password embedded in the DSN, if any.

A resolved password replaces whatever the DSN carries. A reference that resolves to an empty value
is an error, and one trailing newline is trimmed, since files usually end in one.

## Reference schemes

References are resolved by [mamori](https://github.com/xavidop/mamori). The schemes compiled into the
binary are:

| Reference | Reads |
|---|---|
| `env:NAME` | The environment variable `NAME`. |
| `file:///path/to/secret` | A file. |
| `dotenv://.env#KEY` | The key `KEY` in a dotenv file, relative to the working directory. `dotenv:///abs/path/.env#KEY` is the absolute form. The `#KEY` part is required: without it the whole file is returned as the password. |
| `keyring:NAME` | The password stored in the OS keyring under service `tobira`, account `NAME`. |

A literal password in `password:` is a load error, and `tobira connections add` and the TUI form
accept only these references, and refuse a DSN that carries a password. A password written into
a DSN by hand still works, but it is masked in every output and `tobira doctor` warns about it;
move it to `password:`. See [Managing connections](/docs/connections/).

Other secret managers, such as `op://` (1Password), `aws-sm://` and `aws-ps://` (AWS), `vault://`
and the Google Cloud and Azure providers, are not supported. A connection that uses one fails
with "unknown ref scheme" when it opens. Export the secret as `TOBIRA_CONN_<NAME>_PASSWORD` from your
own tooling instead, or use `env:`, `file://` or `keyring:`.

## The OS keyring

`keyring:NAME` reads the macOS Keychain, the Windows Credential Manager or the Secret Service on
Linux. Store a password there with:

```sh
tobira connections set-password local-pg
```

This asks for the password twice with no echo, stores it under service `tobira` and account
`local-pg`, and sets the connection's `password:` to `keyring:local-pg`. Only that line of the
config changes: the entry's other fields and comments are kept, and it works on an entry that did
not load. If the connection had a different reference, one line on stderr says `password ref
changed from <old> to keyring:local-pg`. The password is never accepted as an argument and never
printed or logged.

| Flag | Does |
|---|---|
| `--no-config` | Store the password and leave the config file alone. Then `<name>` need not be a configured connection. |
| `--stdin` | Read the password from one line of standard input instead of prompting. Needed without a terminal, and refused on a terminal, where it would echo. |
| `--force` | Store even when another connection uses the same entry (see below). |

- Without a terminal and without `--stdin`, `set-password` exits 1 and says so. An empty password is
  refused.
- A name must be a valid connection name (letters, digits, `.`, `_` and `-`), and the config file
  must not have changed since it was read.
- When another connection's `password:` is also `keyring:<name>` (a connection renamed from
  `<name>` keeps its reference), storing would change that connection's password too, so
  `set-password` refuses before prompting: `keyring:<name> is used by connection <other>; pass
  --force to overwrite`. With `--no-config` the config is still read for this check.
- Removing a connection (`tobira connections rm`, or `D` in the TUI) also deletes its keyring entry
  when its reference is exactly `keyring:<name>`. If the entry cannot be deleted you get a warning
  and the connection is still removed.
- A `keyring:` reference whose entry does not exist fails when the connection opens (exit 2).

Anyone who can run programs as you can read what the keyring holds. It keeps the password out of
the config file, your dotfiles and your shell history, not out of reach of your own account.
