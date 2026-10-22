// Package redis is the tobira driver for Redis, built on
// github.com/redis/go-redis/v9. Importing it registers the "redis" driver in
// driver.Default for the redis and rediss schemes.
//
// The full reference is https://xavidop.github.io/tobira/docs/drivers/redis/
//
// Namespaces are the logical databases; collections are "*" plus the key
// prefixes found by a bounded SCAN sample. A prefix of hashes is a record
// collection whose fields become typed columns. Every key listing is a
// bounded, cancellable SCAN; the driver never sends KEYS.
//
// Read-only is a fail-closed guard in two layers (guard.go): a command
// allow-list checked before anything is sent, then the server's COMMAND INFO
// flags.
package redis
