# tobira Cloudflare D1 driver

The `d1` driver lets [tobira](https://github.com/xavidop/tobira) connect to
[Cloudflare D1](https://developers.cloudflare.com/d1/) databases through the D1 REST API. It is
built into the `tobira` binary. The SQL is SQLite's.

## Connect

| Form | Example |
| --- | --- |
| `d1://<account_id>/<database_id>` | `d1://0123456789abcdef0123456789abcdef/6f1c2b3a-0d4e-4f5a-9b8c-7d6e5f4a3b2c` |
| with a custom API base | `d1://acct/db?endpoint=https://proxy.example.com/client/v4` |

Put a Cloudflare API token with D1 access in the connection's `password:` (a secret reference) or
`TOBIRA_CONN_<NAME>_PASSWORD`; a token in a query parameter is refused. An `http://` endpoint to
a host other than your machine also needs `allow_plaintext=1`, and is always refused for
`*.cloudflare.com`.

## Quick example

```sh
tobira query "d1://0123456789abcdef0123456789abcdef/6f1c2b3a-0d4e-4f5a-9b8c-7d6e5f4a3b2c" \
  "select * from users limit 5" --read-only
```

## Capabilities

| Capability | Supported |
| --- | --- |
| Query | yes |
| Browse | yes |
| Edit rows | yes |
| Explain | yes (`EXPLAIN QUERY PLAN`) |
| Completion | yes |
| Transactions | no (the REST API keeps no session) |
| Read-only guard | yes |

## Read-only

A read-only call must be one `SELECT`, `WITH`, `VALUES`, `EXPLAIN` or reading pragma, with no
`INSERT`, `UPDATE`, `DELETE` or `REPLACE INTO` inside it. tobira checks this before sending
anything; D1 has no server-side read-only mode. Use a **D1 Read** token for read-only
connections.

## Compatibility

Tested against a local stand-in for the D1 REST API. Not yet tested against Cloudflare.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/d1/>

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/d1"
```
