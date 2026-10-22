---
layout: ../../../layouts/DocsLayout.astro
title: SingleStore
---

# SingleStore

The `singlestore` driver connects to SingleStore. It is built into the `tobira` binary and shares
the [MySQL driver](/docs/drivers/mysql/)'s code, so that page's sections on connecting, TLS,
browsing, editing and completion apply. SingleStore has no transactions and no read-only access
in tobira. This page lists what differs.

## Quick start

```sh
tobira query "singlestore://admin@svc-abc.singlestore.com:3306/app?tls=true" "select now()"
```

```yaml
connections:
  - name: s2
    dsn: singlestore://admin@svc-abc.singlestore.com:3306/app?tls=true
    password: env:S2_PASSWORD
```

## Connecting

| DSN | Result |
| --- | --- |
| `singlestore://user@host:3306/db` | SingleStore, through the `singlestore` driver |
| `mysql://user@host:3306/db?flavor=singlestore` | the same, through the `mysql` driver |
| `mysql://user@host:3306/db` | tobira asks the server when it connects and detects SingleStore |

## Capabilities

| Capability | Supported |
| --- | --- |
| Query | yes |
| Browse | yes (`information_schema`, `memsql` and `cluster` are hidden) |
| Edit rows | yes |
| Explain | yes (`EXPLAIN JSON` as a tree, plus the `EXPLAIN` text; never `PROFILE`) |
| Completion | yes |
| Transactions | no |
| Read-only guard | no |

## No read-only access

SingleStore has no read-only transactions. It accepts `transaction_read_only=1` and
`SET SESSION TRANSACTION READ ONLY` and still writes, and `START TRANSACTION READ ONLY` is a syntax
error. tobira's guard needs such a transaction, so read-only connections, `--read-only` calls and
read-only transactions are refused as unsupported.

To browse a production cluster safely, connect as a user with only `SELECT` grants.

## No transactions

tobira cannot check after a statement that a SingleStore transaction is still open, so
`:tx begin` is refused. Each row edit still runs in its own transaction, and an update or delete
still changes exactly one row.

## Other differences

- SingleStore ignores the session time zone, so `DATETIME` values are shown as stored.
- Unique keys must contain the shard key.
- An index listed once per index type is shown once, without the shard key's own rows.
- A `PERSISTED` computed column is shown as generated, with its expression.
- `CURRENT_TIMESTAMP` defaults are shown as expressions.

## Compatibility

Tested by running the MySQL 8.4 and MariaDB 11.8 suites in SingleStore mode
(`flavor=singlestore`). Not yet tested against a real SingleStore server. If something fails
there, please open an issue.
