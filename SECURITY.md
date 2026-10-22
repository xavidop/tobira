# Security Policy

## Reporting a vulnerability

Please report security vulnerabilities **privately**. Do not open a public issue, discussion, or pull request for a security problem.

Use GitHub's private vulnerability reporting: **[Report a vulnerability](https://github.com/xavidop/tobira/security/advisories/new)**. This opens an advisory visible only to you and the maintainers.

Include as much as you can:

- A description of the issue and its impact.
- Steps to reproduce, or a proof of concept.
- The affected module(s) and version(s).
- Any suggested remediation.

Please do not include real credentials or database contents in your report.

## What to expect

- Acknowledgement within 3 business days.
- An initial assessment and severity rating within 7 days.
- Coordinated disclosure: we will work with you on a fix and a release, and credit you (if you wish) in the advisory and release notes.

## Supported versions

tobira is a multi-module repository: the core module, the CLI, each driver module, the plugin SDK
and the AI module have their own versions. Security fixes land on the latest release of the
affected module, so upgrade that module to its newest version.

| Module | Supported |
| --- | --- |
| core (`github.com/xavidop/tobira`) | latest release |
| `cmd/tobira` (the CLI and TUI) | latest release |
| `drivers/*` (each driver module) | latest release |
| `plugin/sdk` (the Go SDK for driver plugins) | latest release |
| `ai` (the AI assistant and the MCP server) | latest release |

## Scope

In scope:

- The core library, the CLI, the driver modules, the plugin SDK and the `ai` module in this repository.
- Credential handling and redaction: unintended exposure of connection strings, passwords or tokens through logs, errors, or serialization.
- The read-only guard: any way a statement that mutates data gets through a connection that has not opted in to writes.
- The AI assistant: any way it reaches a connection or tool it should not, writes without your
  confirmation, sends row data that `ai.send_rows` forbids, or leaks a key or credential.
- The MCP server: any way a client reaches a connection or tool outside `mcp.expose`, writes outside `mcp.allow_writes` or on a read-only connection, gets row data when `mcp.send_rows` is `never`, or gets past the HTTP token, Host and Origin checks.
- Plugin handling: how an out-of-process plugin is discovered, verified and started, what it is given, and what it can make the host do. A malicious plugin is, by design, code that runs as you; a report that a plugin you chose to install can use your account is out of scope, but any way around the protections below is in scope.

Out of scope:

- Vulnerabilities in third-party database SDKs or the databases themselves. Report those upstream; we will update our dependency once a fix is available.
- Issues that require an already-compromised host, or credentials the reporter already controls.

## Security model

This is a summary. The [security page](https://xavidop.github.io/tobira/docs/security/) of the
docs site has the full model, including each driver's read-only layers and how each one sends
credentials.

- **Passwords are references.** The config file accepts `env:`, `file://`, dotenv and `keyring:`
  references, never a literal password. `tobira connections add` and the TUI form refuse a DSN
  that carries a password. A password written into a DSN by hand still works, but it is masked in
  every output and `tobira doctor` warns about it; move it to `password:`. tobira writes the
  config file with mode 0600.
- **Credentials stay out of output.** Errors, logs, the TUI, the query history and `tobira doctor`
  mask passwords and DSN credentials. Treat the history file
  (`$XDG_STATE_HOME/tobira/history.db`, mode 0600) like a shell history: a secret typed into a
  query in an unusual form may not be masked.
- **Read-only connections.** Set `read_only: true` on a connection, or pass `--read-only`, and
  tobira refuses any statement that writes, whatever its text says. Where the database allows it,
  the database session enforces this too. Every driver except SingleStore has a guard;
  SingleStore has no read-only transaction, so tobira refuses read-only connections to it.
  Connections are writable by default.
- **What the guard cannot stop.** The guard checks statements before they run. It cannot see
  inside code that already lives on the server: user-defined functions, stored procedures, remote
  or external tables and linked servers. It protects against accidental writes; it is not a
  sandbox. For untrusted use, connect as a database user that can only read.
- **Encryption in transit.** The HTTP and gRPC drivers, Cassandra, Vertica and ClickHouse refuse
  to send a credential in plain text to a host that is not this machine unless you add
  `allow_plaintext=1`, and managed cloud services always use TLS. PostgreSQL, MySQL, MongoDB,
  Redis, SQL Server and Oracle follow their client library's TLS settings in the DSN, so set them
  for a remote server. No HTTP driver follows a redirect with a credential.
- **Everything shown is escaped.** Control, bidirectional and zero-width characters in values,
  names and errors print as escapes, so a value cannot rewrite your terminal.
- **The AI assistant** is off until you add an `ai:` section. The provider you choose receives the
  schema, your question and, if `ai.send_rows` allows it, row data. Its query tools always run
  read-only. It writes only in the TUI, after `:ai execute on`, and you confirm each statement.
  `send_rows: never` is not a hard boundary against a hostile model: a withheld result still shows
  its row count and timing. Keep secrets away from connections a model can query.
- **The MCP server** exposes only the connections you name in `mcp.expose` or `--expose`. Reads
  run read-only with a row cap. `execute` exists only for connections in `mcp.allow_writes`, with
  no per-call confirmation, so allow writes only where an unattended agent may write. Over HTTP it
  listens on loopback unless you pass `--insecure-bind`, and it checks a bearer token, the Host
  and the Origin on every request.
- **Driver plugins** are programs that run as you, and tobira does not sandbox them. Install only
  plugins you trust. `tobira driver install` requires a `--sha256` checksum and never runs the
  binary. tobira refuses world-writable plugin binaries, passes a plugin only an allow-list of
  environment variables, and gives it a connection's password only when the connection opens. A
  plugin's read-only guard is the plugin's own promise.
- **Connection import** never writes a password into the config. It offers to store a password it
  finds in the OS keyring, and it never reads DBeaver's encrypted credentials file or the TablePlus
  keychain.
- **Export files** from `tobira query --output` and `:export` are written with mode 0600.
