# embedding

tobira's driver layer used as a library inside another Go program: no TUI, no CLI, no config
file. The example builds a private registry with the one driver it links in
([`drivers/sqlite`](../../drivers/sqlite)), opens a connection read-only, lists its collections
and streams the rows of one query.

```sh
go run .                                                     # a demo SQLite database in a temp dir
go run . -dsn sqlite:./app.db -query 'select * from users' -limit 20
```

Flags: `-dsn` (default: a demo database with a `users` table, created and removed by the run),
`-query` and `-limit` (default 100, `0` for no limit).

What it shows:

- **Only the stable surface.** It imports [`api`](../../api), the stdlib-only package of aliases
  that tobira keeps stable for embedders and plugins, plus the driver modules it wants. The root
  module never pulls a database SDK, so a program that links only SQLite carries only SQLite.
- **A private registry.** `api.NewRegistry()` holds exactly the drivers the program adds;
  `reg.Open` picks one by the DSN scheme (or `ConnConfig.Driver`). Add another driver module, or
  your own ([`custom-driver-module`](../custom-driver-module)), with `reg.Add`.
- **Read-only twice.** `ConnConfig.ReadOnly` opens the database read-only and `Query.ReadOnly`
  refuses anything that writes, with an error that matches `api.ErrReadOnly`.
- **Streaming and redaction.** Rows come one at a time from `ResultSet.Next`, values print with
  `Value.String`, and errors go through `api.RedactMessage`, since a DSN may carry a password.

`go test ./...` runs it against temp SQLite databases. The driver contract is described on the
[Concepts](https://xavidop.github.io/tobira/docs/concepts/) and
[Write a driver](https://xavidop.github.io/tobira/docs/write-a-driver/) pages.
