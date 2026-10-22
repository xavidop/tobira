# tobira MySQL driver

The `mysql` driver lets [tobira](https://github.com/xavidop/tobira) connect to MySQL. It is built
into the `tobira` binary and uses `github.com/go-sql-driver/mysql`. The same package also
provides the `mariadb` and `singlestore` drivers.

## Connect

| Form | Example |
| --- | --- |
| MySQL URL | `mysql://user@db.internal:3306/app?tls=true` |
| Unix socket | `mysql://user@localhost/app?socket=/var/run/mysqld/mysqld.sock` |
| MariaDB | `mariadb://user@db.internal:3306/app` |
| SingleStore | `singlestore://user@host:3306/app` |
| go-sql-driver form (config file, with `driver: mysql`) | `user@tcp(localhost:3306)/app` |

TLS is off unless you set `tls=true` (verified) or `tls=skip-verify`. Keep the password in the
connection's `password:` reference or `TOBIRA_CONN_<NAME>_PASSWORD`. Settings go in the DSN;
the connection's `options:` are refused. `multiStatements`, `interpolateParams` and charsets
other than `utf8mb4` are refused.

## Quick example

```sh
tobira query "mysql://reader@db.internal:3306/app?tls=true" "select now()" --read-only
```

## Capabilities

| Capability | MySQL | MariaDB | SingleStore |
| --- | --- | --- | --- |
| Query, browse, explain, completion | yes | yes | yes |
| Edit rows | yes | yes | yes |
| Transactions | yes | yes | no |
| Read-only guard | MySQL 8.0 to 8.4 | MariaDB 11.1 to 11.x | no |

## Read-only

A read-only call passes an allow-list (`SELECT`, `WITH`, `VALUES`, `TABLE`, `SHOW`, `DESCRIBE`,
`EXPLAIN`, `USE`) before anything is sent, runs inside `START TRANSACTION READ ONLY ... ROLLBACK`,
and is checked afterwards to prove it stayed inside that transaction. Executable comments and
`INTO OUTFILE` are refused. On a server outside the verified versions, and on SingleStore, a
read-only connection fails to open. Federated and remote tables can still act on another server,
so use a `SELECT`-only user for untrusted work.

## Compatibility

- MySQL: tested with MySQL 8.4.
- MariaDB: tested with MariaDB 11.8.
- SingleStore: tested by running the MySQL and MariaDB suites in SingleStore mode. Not yet tested
  against a real SingleStore server.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/mysql/>, with
[MariaDB](https://xavidop.github.io/tobira/docs/drivers/mariadb/) and
[SingleStore](https://xavidop.github.io/tobira/docs/drivers/singlestore/).

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/mysql"
```
