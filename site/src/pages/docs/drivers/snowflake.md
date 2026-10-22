---
layout: ../../../layouts/DocsLayout.astro
title: Snowflake
---

# Snowflake

The `snowflake` driver connects to Snowflake. It is built into the `tobira` binary and uses the
official Go driver, `github.com/snowflakedb/gosnowflake/v2`. You can query, browse, edit rows,
explain queries and run transactions.

**Not yet tested against Snowflake.** The driver is tested against
[fakesnow](https://github.com/tekumara/fakesnow), a fake of Snowflake backed by DuckDB, and a
local fake of Snowflake's REST endpoints. Please open an issue if something fails against a real
account.

## Quick start

```sh
tobira query "snowflake://ada@myorg-myaccount/SALES/PUBLIC?warehouse=COMPUTE_WH" "select count(*) from orders" --read-only
```

```yaml
connections:
  - name: sf
    dsn: snowflake://ada@myorg-myaccount/SALES/PUBLIC?warehouse=COMPUTE_WH&role=ANALYST
    password: keyring:snowflake-ada
    read_only: true
```

## Connecting

```text
snowflake://user@account[/database[/schema]][?warehouse=WH&role=ROLE&authenticator=...]
snowflake://ada@myorg-myaccount/SALES/PUBLIC?warehouse=COMPUTE_WH&role=ANALYST
snowflake://svc@xy12345.eu-west-1/SALES?authenticator=snowflake_jwt
```

- The host is the **account identifier** (`orgname-accountname`, or `locator.region`), not the
  full `snowflakecomputing.com` name.
- The path is the database and schema, percent-encoded when needed. Names keep their case.
- A user is required.

| Parameter | Effect |
| --- | --- |
| `warehouse` | The session's warehouse. |
| `role` | The session's role. |
| `authenticator` | What the password holds (see below). |
| `host`, `port` | A custom endpoint, such as a private link. |
| `protocol` | `https` (the default) or `http`. |
| `allow_plaintext` | Needed with `protocol=http` to a host other than your machine. |

Any other parameter is refused, and so are the connection's `options:`.

### Credentials

Put the credential in the connection's `password:` as a [secret reference](/docs/secrets/), or in
`TOBIRA_CONN_<NAME>_PASSWORD`. A DSN that carries a credential is refused. `authenticator=` says
what the password holds:

| `authenticator` | The password holds |
| --- | --- |
| `snowflake` (default) | the user's password |
| `snowflake_jwt` | an unencrypted RSA private key in PEM, for key-pair authentication |
| `oauth` | an OAuth access token |
| `programmatic_access_token` | a programmatic access token |

Browser SSO, Okta and MFA prompts are not supported. tobira never caches ID or MFA tokens in your
keychain. To store an encrypted key unencrypted, run `openssl pkcs8 -topk8 -nocrypt`.

### TLS and `allow_plaintext=1`

Connections use HTTPS, with certificates checked against your system's roots. `protocol=http` to
a host other than your machine also needs `allow_plaintext=1`, and is always refused for
Snowflake's own endpoints (`*.snowflakecomputing.com`, `*.snowflakecomputing.cn`). tobira never
follows an HTTP redirect, so your login cannot be replayed to another host.

tobira turns off the Snowflake SDK's own logging and its cloud platform detection, so the SDK
writes nothing to your terminal and does not probe cloud metadata servers.

## Capabilities

| Capability | Supported |
| --- | --- |
| Query | yes |
| Browse (filter, sort, paging) | yes (namespaces are `database.schema`) |
| Edit rows | yes, by primary key or any declared key |
| Transactions | yes (`READ COMMITTED`) |
| Explain | yes (`EXPLAIN USING TEXT`; compiled, never run; no `ANALYZE`) |
| Completion | yes (keywords, schemas, tables, columns) |
| Read-only guard | yes |

## Queries

Write Snowflake SQL, one statement per query. Parameters bind to `?` placeholders.

```sh
tobira query sf "select region, sum(amount) from orders group by region"
tobira query sf --explain "select * from orders where customer_id = 7"
```

- Each query can run on a different session, so `USE` and `SET` do not carry over to the next
  query. A transaction keeps one session.
- Cancelling a query returns at once and asks Snowflake to abort it.

## Browsing

Pages use `LIMIT` and `OFFSET`, ordered by your sort and then the primary key.

## Editing

Edit rows in the grid (`e`, `i`, `d`, then `:apply`). See [Editing rows](/docs/editing/).

Snowflake records primary keys but does not enforce them, so tobira does:

- An insert only adds the row if no row has the same key; a duplicate fails as a constraint
  error. This check is not a lock, so two clients inserting the same key at once can both
  succeed. A key filled by `AUTOINCREMENT` is not shown back.
- An update or delete runs in its own transaction and is rolled back unless it changed exactly
  one row.
- Inside a `:tx` transaction, editing is not available, because Snowflake has no savepoints.

## Transactions

Snowflake commits the open transaction before any DDL statement. So inside a transaction tobira
refuses DDL, `TRUNCATE`, `CALL`, `EXECUTE IMMEDIATE`, `USE`, `COMMIT` and `ROLLBACK`. Use
`:tx commit` and `:tx rollback`.

## Read-only guard

A connection is read-only when it has `read_only: true`, when you pass `--read-only`, or when you
run `:readonly on`. Snowflake has no read-only session or transaction mode, so tobira checks every
statement before it is sent, and limits each request to one statement. A read-only statement must
be one `SELECT`, `WITH`, `SHOW`, `DESCRIBE` or `EXPLAIN`, and tobira refuses:

- a second statement, and the flow operator `->>`, which chains statements;
- stage references (`@stage`, `@~`, `@%t`);
- `INSERT`, `UPDATE`, `DELETE`, `MERGE`, `CALL`, `PROCEDURE`, `EXECUTE IMMEDIATE`, DDL, `COMMENT`,
  `COPY`, `PUT`, `INTO`, `NEXTVAL`, `INFER_SCHEMA` and `IDENTIFIER` anywhere outside strings and
  quoted names (this also stops `WITH ... DELETE` and `EXPLAIN` of a write);
- `SYSTEM$` functions, except `SYSTEM$TYPEOF`;
- table functions other than `GENERATOR`, `RESULT_SCAN`, `FLATTEN`, `SPLIT_TO_TABLE`,
  `STRTOK_SPLIT_TO_TABLE` and the `INFORMATION_SCHEMA` ones, because a procedure selected with
  `FROM TABLE(proc(...))` can run DML;
- text tobira cannot read to its end.

Refused statements make `tobira query` exit with code 4. Row edits are refused too.

**What it cannot stop.** The check runs only in tobira; Snowflake has no read-only layer. A
user-defined or external function called from a `SELECT` can still reach outside Snowflake. For
a server-side guarantee, connect with a role that holds only `SELECT` and `USAGE` grants.

## Types

| Snowflake | tobira type |
| --- | --- |
| `NUMBER` with scale 0 | integer (decimal beyond 64 bits) |
| `NUMBER(p,s)` | decimal (exact) |
| `FLOAT` | float |
| `DATE`, `TIMESTAMP_*` | time (`TIMESTAMP_NTZ` read as UTC) |
| `TIME` | text |
| `VARIANT`, `OBJECT`, `ARRAY` | JSON |
| `BINARY` | bytes |
| `GEOGRAPHY` | text |
| `VECTOR` | vector |

A value over 1 MiB is cut, with a warning on the result.

## Limits

- One statement per query.
- No `ANALYZE` plans and no native export.
- No editing inside a transaction.
- Values are cut at 1 MiB.

## Compatibility

Tested against fakesnow 0.11, a local Snowflake emulator, for queries, browsing, editing,
transactions, types and the read-only guard, and against a local stand-in for Snowflake's API for
`EXPLAIN`, every authenticator, bad credentials and cancellation. Not yet tested against Snowflake
itself, including its own error numbers and large results sent in chunks.

## Troubleshooting

| Message | What to do |
| --- | --- |
| `credentials must not be in the DSN` | Move the password, key or token to the connection's `password:`. |
| `malformed DSN: unknown parameter` | Only the parameters in the table above are accepted. |
| `malformed DSN: a user is required` | Add the user: `snowflake://user@account`. |
| `unsupported authenticator` | Use `snowflake`, `snowflake_jwt`, `oauth` or `programmatic_access_token`. |
| `the private key is encrypted` | Store it unencrypted in the secret store (`openssl pkcs8 -topk8 -nocrypt`). |
| `plain http to a host that is not loopback needs allow_plaintext=1` | Use `https`, or add `allow_plaintext=1`. |
| `authentication failed` | Check the user, the account identifier and the credential. |
