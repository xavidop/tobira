# tobira Redis driver

The Redis driver for [tobira](https://github.com/xavidop/tobira), built on
`github.com/redis/go-redis/v9`. It registers the `redis` driver for the
`redis://`, `rediss://` and `unix://` schemes and is compiled into the
`tobira` binary.

## Connect

```text
redis://localhost:6379/0
redis://user@cache.example.com:6379/2?dial_timeout=3s
rediss://cache.example.com:6380/0
unix:///var/run/redis.sock?db=1
```

The path is the database number. `rediss://` uses TLS. A `unix://` socket
needs `driver: redis` on the connection. Query parameters are go-redis's
(`dial_timeout`, `read_timeout`, `pool_size`, `client_name`, `skip_verify`
and so on); an unknown one is refused. Cluster mode is not supported.

## Quick example

A query is one Redis command, quoted as in `redis-cli`.

```sh
tobira query "redis://localhost:6379/0" "HGETALL user:1" --read-only
tobira query "redis://localhost:6379/0" "ZRANGE leaderboard 0 -1 WITHSCORES"
```

## Capabilities

| Capability | Support |
| --- | --- |
| Query, databases, collections, describe | yes; databases are namespaces, key prefixes are collections |
| Browse | yes; key, type, TTL and a value preview, paged with `SCAN` |
| Editing | yes; insert, update and delete of one key at a time |
| Watching | yes; keyspace notifications, when the server has them on |
| Explain, transactions, completion, native export | no |

## Read-only

On a read-only connection only an allow-list of read commands runs (`GET`,
`HGETALL`, `LRANGE`, `SCAN`, `INFO`, `FT.SEARCH` and the like). Scripts,
`CONFIG`, `KEYS`, `SELECT`, `MULTI` and every write are refused. tobira also
asks the server how it classifies each command and refuses any write or admin
command, which catches a `rename-command`.

## Compatibility

Tested with Redis 8, including RedisJSON and RediSearch.

## Documentation

Full reference: <https://xavidop.github.io/tobira/docs/drivers/redis/>

Go programs can import the driver directly:

```go
import _ "github.com/xavidop/tobira/drivers/redis"
```
