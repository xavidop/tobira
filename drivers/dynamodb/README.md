# tobira DynamoDB driver

The Amazon DynamoDB driver for [tobira](https://github.com/xavidop/tobira),
built on `aws-sdk-go-v2`. It registers the `dynamodb` driver for the
`dynamodb://` scheme and is compiled into the `tobira` binary.

## Connect

```text
dynamodb://us-east-1                                       # the AWS default credential chain
dynamodb://eu-west-1?profile=prod                          # a shared-config profile
dynamodb://AKIAIOSFODNN7EXAMPLE@us-east-1                  # an access key; the secret is the password
dynamodb://local@us-east-1?endpoint=http://localhost:8000  # DynamoDB Local
```

The host is the region. Parameters: `endpoint`, `profile` and
`allow_plaintext`. An `http://` endpoint on another machine needs
`allow_plaintext=1`; AWS's own hosts (`*.amazonaws.com`,
`*.amazonaws.com.cn`) are always HTTPS.

## Quick example

A query is one PartiQL statement.

```sh
tobira query "dynamodb://eu-west-1?profile=prod" \
  "SELECT * FROM \"orders\" WHERE customer = 'c-42'" --read-only
```

## Capabilities

| Capability | Support |
| --- | --- |
| Query, tables, describe | yes; describe reads keys and indexes and samples 100 items |
| Browse | yes; a scan paged by key; sorting up to 10,000 items |
| Editing | yes; insert, update and delete of one item, by its full primary key |
| Completion | yes; PartiQL keywords and functions, tables and attributes |
| Watching | yes; DynamoDB Streams, on a table with its stream enabled |
| Explain, transactions, native export | no |

## Read-only

On a read-only connection only a single PartiQL `SELECT` runs. A statement
with a write word (`INSERT`, `UPDATE`, `DELETE`, `SET`, `REMOVE`, `EXISTS` and
others) outside quotes, an Ion literal, or a second statement is refused
before it is sent. Edits are refused too. Pair it with a read-only IAM
identity.

## Compatibility

Tested with DynamoDB Local 3.3. Not yet tested against AWS: authentication, IAM, throttling,
eventual consistency and stream shard splits are not emulated by DynamoDB Local.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/dynamodb/>

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/dynamodb"
```
