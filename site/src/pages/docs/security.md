---
layout: ../../layouts/DocsLayout.astro
title: Security
---

# Security

This section explains what tobira does to protect your credentials and data, and what it cannot
stop. Start here for credentials kept on your machine; the subpages cover the read-only guard,
credentials on the network, the AI assistant and MCP, and plugins. The policy for reporting a
vulnerability is in [SECURITY.md](https://github.com/xavidop/tobira/blob/main/SECURITY.md).

## In this section

| Page | Covers |
|---|---|
| [Read-only guard](/docs/security/read-only/) | What `read_only: true` does on each driver, and what it does not stop |
| [Network credentials](/docs/security/network/) | TLS defaults, `allow_plaintext=1`, redirects and each driver's cloud endpoints |
| [AI and MCP](/docs/security/ai-and-mcp/) | What is sent to a model, and what an MCP client can reach |
| [Plugins](/docs/security/plugins/) | How far an external driver plugin is trusted |

## Reporting a vulnerability

Report it **privately** through GitHub's
[private vulnerability reporting](https://github.com/xavidop/tobira/security/advisories/new), never
in a public issue. Include the impact, steps to reproduce and the affected module and version, and
no real credentials or database contents. Expect an acknowledgement within 3 business days and an
assessment within 7. Only the latest release of each module gets security fixes.

## Credentials on your machine

- **Passwords are references.** The config file and the connection form accept `env:`, `file://`,
  dotenv and `keyring:` references, never a literal password. `tobira connections add` and the
  connection form refuse a DSN that carries a password. A password written into a DSN by hand
  still works, but it is masked in every output and `tobira doctor` warns about it; move it to
  `password:`.
  `TOBIRA_CONN_<NAME>_PASSWORD` overrides a reference. The config file is written with mode 0600.
  See [Passwords and secrets](/docs/secrets/).
- **The OS keyring.** `tobira connections set-password` stores a password in the macOS Keychain,
  the Windows Credential Manager or the Secret Service, under service `tobira`. It reads the
  password with no echo (or one line of stdin with `--stdin`), never as an argument. Anything that
  runs as you can read the keyring: it keeps the password out of files and shell history, not out
  of reach of your account.
- **Redaction.** Errors, warnings, the terminal UI and the history pass through one redactor. It
  masks DSN passwords (URL, key=value, MySQL `user:pw@` and ADO/ODBC forms), the resolved password
  and `password=`, `password: x` and `"password": "x"` forms. A bare password shorter than 4 bytes
  is masked only where it is named, since it cannot be told apart from ordinary text. Debug logs
  never record passwords or error text.
- **History.** The [query history](/docs/history/) (mode 0600, last 10,000 entries) stores each
  statement after the redactor ran over it. A secret typed into the query text in another shape is
  not recognised, so treat the file like a shell history.
- **Export files** from `tobira query --output` and `:export` are written atomically with mode
  0600.
- **Escaping.** Control, bidirectional and zero-width characters in database values, names and
  errors are printed as escapes, so a value cannot rewrite your terminal.
- **`tobira doctor`** reports a secret reference as ok or fail with its scheme only, never the
  value, its length or the rest of the reference. Without `--online` it makes no network call
  except the database pings and remote references; `--no-ping` skips those too.
- **Connection import** is a dry run until `--apply`. A password it finds (pgpass, or DBeaver's
  plain JSON fields) goes to the OS keyring and is never written into the config. DBeaver's
  encrypted `credentials-config.json` and the TablePlus keychain entries are never read, and a
  `~/.pgpass` that is group- or world-readable is refused. `tobira connections export` keeps
  references and strips every password from DSNs. See
  [importing connections](/docs/connections/import/).

## The short version

- Use `read_only: true` for any connection you only read, and pair it with a database user that can
  only read. The [read-only guard](/docs/security/read-only/) stops accidental writes; a
  least-privilege user is the real boundary.
- Keep TLS on. Most drivers refuse a credential in plain text to another machine unless you add
  `allow_plaintext=1`, and managed clouds always use TLS. For PostgreSQL, MySQL, MongoDB, Redis,
  SQL Server and Oracle, turn TLS on in the DSN for a remote server; see the
  [network page](/docs/security/network/).
- Choose the AI provider like any third-party service, and set `ai.send_rows: never` for data that
  must not leave.
- Expose to MCP only the connections an agent should see.
- Install only plugins you trust.
