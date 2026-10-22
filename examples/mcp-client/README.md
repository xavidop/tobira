# mcp-client

The smallest MCP client for `tobira mcp`, written with
[mcp-go](https://github.com/mark3labs/mcp-go). It starts `tobira mcp --expose <connection>` over
stdio, lists the tools, calls `describe` and prints the result.

```sh
go run . -connection local-pg -collection users
go run . -tobira ./tobira -config ./config.yaml -connection lite -namespace main -collection notes
```

Flags: `-tobira` (the binary, default `tobira` from `PATH`), `-config`, `-connection` and
`-collection` (required) and `-namespace`. The connection must exist in the tobira config; it is
the only one the server exposes, and `execute` is not offered because nothing is allow-listed.

`go test ./...` builds `cmd/tobira` into a temp dir and runs the example against a temp SQLite
database. The guide to the server is on the
[MCP page](https://xavidop.github.io/tobira/docs/mcp/).
