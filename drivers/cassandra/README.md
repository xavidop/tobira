# tobira Cassandra driver

The Apache Cassandra and ScyllaDB driver for
[tobira](https://github.com/xavidop/tobira), built on the Apache gocql driver
(`github.com/apache/cassandra-gocql-driver/v2`). It registers the `cassandra`
driver for the `cassandra://` and `scylla://` schemes and is compiled into the
`tobira` binary.

## Connect

```text
cassandra://localhost/shop
cassandra://reader@cass1:9042,cass2:9042/shop?consistency=LOCAL_ONE&dc=eu-west
cassandra://reader@cass.example.com/shop?tls=true&tls_ca=/etc/ssl/cass-ca.pem
scylla://reader@scylla1/shop?proto=4
```

The port defaults to 9042 and the path is the default keyspace. Parameters:
`consistency`, `serial_consistency`, `timeout`, `connect_timeout`, `dc`,
`tls`, `tls_ca`, `tls_server_name`, `tls_insecure`, `proto`, `discovery`,
`system_keyspaces` and `allow_plaintext`. Without TLS, a password goes to a
host that is not this machine only with `allow_plaintext=1`, and never to
Amazon Keyspaces.

## Quick example

Queries are CQL.

```sh
tobira query "cassandra://reader@localhost/shop" \
  "SELECT * FROM orders WHERE tenant = 'acme'" --read-only
```

## Capabilities

| Capability | Support |
| --- | --- |
| Query, keyspaces, tables, describe | yes; describe shows partition and clustering keys |
| Browse | yes; follows the server's paging state; sorting up to 10,000 rows |
| Editing | yes; one row by its whole primary key, as a lightweight transaction |
| Completion | yes; CQL keywords, keyspaces, tables and columns |
| Explain, transactions, watching, native export | no |

## Read-only

On a read-only connection only one `SELECT`, `DESCRIBE` or `DESC` statement
runs, calling built-in functions only. Writes, `BATCH`, schema changes,
`GRANT`, `USE`, user-defined functions and a second statement are refused
before anything is sent.

## Compatibility

Tested with Cassandra 5.0, one node, with password authentication. Not yet tested: ScyllaDB,
TLS and multi-node clusters.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/cassandra/>

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/cassandra"
```
