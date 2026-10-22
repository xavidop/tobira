# tobira Vertica driver

The `vertica` driver lets [tobira](https://github.com/xavidop/tobira) connect to Vertica
(OpenText Analytics Database). It is built into the `tobira` binary and uses the official Go
driver, `github.com/vertica/vertica-sql-go`.

## Connect

| Form | Example |
| --- | --- |
| Database | `vertica://dbadmin@vertica.example.com/analytics` |
| Backup nodes | `vertica://ada@10.0.0.5:5433/analytics?backup_server_node=10.0.0.6,10.0.0.7` |
| OAuth | `vertica://vertica.example.com/analytics?auth=oauth` |

The password or OAuth token goes in the connection's `password:` (a secret reference); a DSN with
a credential is refused. Off your machine the default is `tlsmode=server-strict`; `server`,
`prefer` and `none` to a remote host also need `allow_plaintext=1`.

## Quick example

```sh
tobira query "vertica://dbadmin@vertica.example.com/analytics?tlsmode=server-strict" "select count(*) from sales" --read-only
```

## Capabilities

| Capability | Supported |
| --- | --- |
| Query | yes |
| Browse | yes |
| Edit rows | yes |
| Transactions | yes |
| Explain | yes |
| Completion | yes |
| Read-only guard | yes |

## Read-only

A read-only call must be one `SELECT`, `WITH`, `SHOW` or `EXPLAIN`, calling only known built-in
functions. It runs in `START TRANSACTION READ ONLY` and is rolled back. `COPY ... FROM LOCAL` and
`FROM STDIN` are refused on every connection. User-defined functions inside views are not
stopped, so use a `SELECT`-only user for untrusted work.

## Compatibility

Tested with Vertica 25.4, a single node. TLS, OAuth, multi-node and Eon-mode databases have not
been tested.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/vertica/>

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/vertica"
```
