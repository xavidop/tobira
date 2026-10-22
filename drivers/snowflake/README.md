# tobira Snowflake driver

The `snowflake` driver lets [tobira](https://github.com/xavidop/tobira) connect to Snowflake. It
is built into the `tobira` binary and uses the official Go driver,
`github.com/snowflakedb/gosnowflake/v2`.

## Connect

| Form | Example |
| --- | --- |
| Account | `snowflake://ada@myorg-myaccount` |
| Database and schema | `snowflake://ada@myorg-myaccount/SALES/PUBLIC?warehouse=COMPUTE_WH&role=ANALYST` |
| Key pair | `snowflake://svc@xy12345.eu-west-1/SALES?authenticator=snowflake_jwt` |

The host is the account identifier. The credential goes in the connection's `password:` (a
secret reference): a password, an unencrypted PEM key for `snowflake_jwt`, an OAuth token for
`oauth`, or a programmatic access token. Browser SSO, Okta and MFA prompts are not supported.
`protocol=http` to a host other than your machine needs `allow_plaintext=1`, and is always
refused for Snowflake's own endpoints.

## Quick example

```sh
tobira query "snowflake://ada@myorg-myaccount/SALES/PUBLIC?warehouse=COMPUTE_WH" "select count(*) from orders" --read-only
```

## Capabilities

| Capability | Supported |
| --- | --- |
| Query | yes |
| Browse | yes |
| Edit rows | yes |
| Transactions | yes (`READ COMMITTED`) |
| Explain | yes (`EXPLAIN USING TEXT`) |
| Completion | yes |
| Read-only guard | yes |

## Read-only

Snowflake has no read-only mode, so tobira checks every statement before it is sent: one
`SELECT`, `WITH`, `SHOW`, `DESCRIBE` or `EXPLAIN`, with no write or DDL words, stages, `SYSTEM$`
functions or unknown table functions. User-defined functions are not stopped, so use a role with
only `SELECT` and `USAGE` grants for a server-side guarantee.

## Compatibility

Tested against fakesnow 0.11, a local Snowflake emulator. Not yet tested against Snowflake.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/snowflake/>

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/snowflake"
```
