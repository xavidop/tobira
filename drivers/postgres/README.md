# tobira PostgreSQL driver

The `postgres` driver lets [tobira](https://github.com/xavidop/tobira) connect to PostgreSQL. It
is built into the `tobira` binary and uses `github.com/jackc/pgx/v5`. The same package also
provides the `redshift`, `cockroachdb` and `greenplum` drivers for those PostgreSQL-compatible
databases.

## Connect

| Form | Example |
| --- | --- |
| URL | `postgres://user@localhost:5432/app?sslmode=require` |
| URL | `postgresql://user@db.internal/app?sslmode=verify-full` |
| libpq keywords (config file only) | `host=localhost port=5432 dbname=app user=reader sslmode=disable` |
| Amazon Redshift | `redshift://user@cluster:5439/dev?sslmode=verify-full` |
| CockroachDB | `cockroachdb://user@host:26257/app` or `cockroach://...` |
| Greenplum | `greenplum://user@coordinator:5432/db` |

TLS follows libpq's `sslmode` (default `prefer`); use `verify-full` for remote servers. Keep the
password in the connection's `password:` reference or `TOBIRA_CONN_<NAME>_PASSWORD`, not in the
DSN. The `options:` keys `application_name`, `search_path` and `statement_timeout` are accepted.

## Quick example

```sh
tobira query "postgres://reader@db.internal/app?sslmode=verify-full" "select now()" --read-only
```

## Capabilities

| Capability | PostgreSQL | CockroachDB | Greenplum | Redshift |
| --- | --- | --- | --- | --- |
| Query, browse, explain, completion | yes | yes | yes | yes |
| Edit rows, transactions | yes | yes | yes | no |
| Native csv export (`COPY`) | yes | yes | yes | no |
| Watch (`LISTEN`) | yes | no | yes | no |
| Vector search (pgvector) | yes | no | yes | no |
| Read-only guard | yes | yes | yes | yes |

## Read-only

Every call on a read-only connection runs in its own `BEGIN READ ONLY ... ROLLBACK`, one statement
at a time, and sessions start with `default_transaction_read_only=on` (except on Redshift). `COPY`, `DO` blocks and
server-file functions such as `lo_export` are refused before they run. CockroachDB and Redshift
also get an allow-list (`SELECT`, `WITH`, `VALUES`, `TABLE`, `SHOW`, `EXPLAIN`). The guard stops
accidental writes; it is not a sandbox against a superuser, so use a `SELECT`-only role for
untrusted work.

## Compatibility

- PostgreSQL: tested with PostgreSQL 17 and pgvector 0.8.7.
- CockroachDB: tested with CockroachDB 26.2.
- Redshift and Greenplum: tested by running the PostgreSQL suite in Redshift or Greenplum mode.
  Not yet tested against a real cluster.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/postgres/>, with
[Redshift](https://xavidop.github.io/tobira/docs/drivers/redshift/),
[CockroachDB](https://xavidop.github.io/tobira/docs/drivers/cockroachdb/),
[Greenplum](https://xavidop.github.io/tobira/docs/drivers/greenplum/) and
[pgvector](https://xavidop.github.io/tobira/docs/drivers/pgvector/).

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/postgres"
```
