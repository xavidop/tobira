---
layout: ../../../layouts/DocsLayout.astro
title: SSH tunnels
---

# SSH tunnels

A database that is reachable only from a bastion host gets an `ssh:` block on its connection.
tobira opens an SSH session to that host, listens on a loopback port on your machine and forwards
each connection to the host and port the DSN names, as seen from the SSH server. The driver
connects to the loopback port and knows nothing about SSH, so every network driver works the
same way.

```yaml
connections:
  - name: prod-pg
    dsn: postgres://app@db.internal:5432/app   # the database as the bastion sees it
    password: env:PROD_PG_PASSWORD
    ssh:
      host: bastion.example.com                # host or host:port; port 22 by default
      user: deploy                             # the SSH login; your user name by default
      key: ~/.ssh/id_ed25519                   # a private key file (optional, see below)
      passphrase: env:KEY_PASS                 # a secret reference, only for an encrypted key
```

Then use the connection as any other: `tobira query prod-pg "select now()"`, `tobira connect
prod-pg`, or expose it to the AI assistant and the MCP server. The tunnel opens with the
connection and closes with it.

## Setting it up

1. **Make sure `ssh deploy@bastion.example.com` works** from your machine, with a key or a
   password. tobira uses the same files, but it does not read `~/.ssh/config`: a host alias, user,
   port or identity file set there goes in the block.
2. **Add the block** to the connection in the [config file](/docs/configuration/#connections). The
   TUI connection form does not edit it.
3. **Check it** with `tobira doctor`. With `--no-ping` it verifies, without any network, that the
   DSN can be forwarded and that the key and `known_hosts` files exist. Without the flag it opens
   the connection through the tunnel and pings the database.

## The keys

| Key | Meaning |
|---|---|
| `host` | Required. The SSH server, `host` or `host:port`. |
| `port` | The SSH port when it is not in `host`; 22 by default. 1 to 65535. |
| `user` | The SSH login. Defaults to `$USER`. |
| `key` | A private key file (absolute, `~` or `$VAR`). Without it, the agent and the default keys in `~/.ssh` are tried. |
| `passphrase` | A [secret reference](/docs/secrets/) for the key's passphrase, never the value. |
| `password` | A secret reference for SSH password authentication, tried after keys. |
| `known_hosts` | The file the server's host key is checked against (absolute, `~` or `$VAR`); `~/.ssh/known_hosts` by default. |

An unknown key, a missing host, a port outside 1 to 65535, a relative file path or a literal
`passphrase` or `password` makes the connection [broken](/docs/connections/#broken-entries).

## How it authenticates

In this order, stopping at the first the server accepts:

1. `key`, if set, with `passphrase` when the file is encrypted. A key that needs a passphrase and
   has none configured is an error that says so.
2. The SSH agent at `$SSH_AUTH_SOCK`, with every key it holds (`ssh-add` to load one).
3. Without a `key`: `~/.ssh/id_ed25519`, `~/.ssh/id_ecdsa` and `~/.ssh/id_rsa`, those that exist,
   as `ssh` itself does.
4. `password`, if set.

With none of these available the connection fails before any network, with
`no way to authenticate: set ssh.key or ssh.password, or run an SSH agent`.

## Host keys

The server's host key must be in `~/.ssh/known_hosts`, or in the file `known_hosts:` names. tobira
never trusts a host on first use and never writes to the file. If you have connected to the host
with `ssh` before, its key is already there. Otherwise add it once:

```sh
ssh-keyscan -H bastion.example.com >> ~/.ssh/known_hosts
```

An unknown host, a key that does not match (a changed server, or someone in the middle) and a
missing file are each a distinct error. See
[Troubleshooting](/docs/troubleshooting/connections/#an-ssh-tunnel-does-not-open).

## Which DSNs work

The tunnel forwards to exactly one host and port, so the DSN must be of the form
`scheme://[user[:password]@]host[:port]/...` and name one host. That is what every network driver
takes: PostgreSQL and pgvector, MySQL and MariaDB, SQL Server, Oracle, ClickHouse, MongoDB, Redis,
Cassandra, Neo4j, Qdrant, Elasticsearch and OpenSearch, InfluxDB and the other HTTP drivers. The
host is the database's address *as the SSH server sees it*, which is often a private name or
address.

The port can be left out for the common schemes (`postgres`, `mysql`, `sqlserver`, `oracle`,
`clickhouse`, `mongodb`, `redis`, `cassandra`, `neo4j`, `bolt`, `qdrant`, `http`, `https`);
otherwise add it.

These cannot be tunnelled, and the connection says so before opening any SSH session:

- **A DSN that lists several hosts**, such as a MongoDB replica set or a Cassandra cluster. The
  server redirects the client to its members' real addresses, which the tunnel does not reach.
- **`mongodb+srv://`**, which finds its hosts by DNS.
- **A file database** such as SQLite or DuckDB.
- **Managed cloud services**, which are reached directly and need no tunnel.

## TLS inside the tunnel

The driver connects to `127.0.0.1`, so the [loopback rules](/docs/security/network/) apply: a
driver that turns TLS on only for remote hosts connects in clear inside the tunnel, which SSH
encrypts end to end. TLS the DSN asks for is still used, but a mode that checks the certificate
against the host name (`sslmode=verify-full` in PostgreSQL, say) fails, because the name is now
`127.0.0.1`. Use `verify-ca`, or the driver's option for the expected server name.

## What tobira shows

- `tobira connections list` shows `user@host` in an `SSH` column, and `ssh` in its JSON.
- `tobira doctor` names the server in the connection row (`via ssh bastion.example.com`) and
  checks the `passphrase` and `password` references like any other.
- `tobira connections export` and `import` keep the block, with a literal passphrase or password
  dropped the way a literal connection password is.
- With `--debug`, the log records the server, user, target and loopback port of each tunnel. No
  secret is logged.
