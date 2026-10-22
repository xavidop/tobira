---
layout: ../../../layouts/DocsLayout.astro
title: tobira mcp
---

# Serve MCP clients

`tobira mcp` serves chosen connections to an MCP client such as Claude Code, over stdio or over
HTTP. This page is the flag reference. The [MCP server](/docs/mcp/) guide covers setup for each
client, the tools and the security model.

```sh
tobira mcp --expose local-pg
tobira mcp --expose local-pg,cache --allow-writes cache --max-rows 100
tobira mcp --expose local-pg --http 127.0.0.1:8765
```

## tobira mcp

```sh
tobira mcp [--expose <a,b>] [--allow-writes <a>] [--max-rows <n>] [--send-rows always|never] [--http <host:port> [--insecure-bind] [--allowed-host <name>]]
```

Serves tobira's tools (`list_connections`, `list_namespaces`, `list_collections`, `describe`,
`sample`, `query`, `explain`, `similar` on vector databases, and `execute` when writes are allowed)
and the `tobira://<connection>/<namespace>/<collection>/schema` resources. Only exposed connections
are visible; any other name is answered as unknown. `execute` exists only when some exposed
connection is in `allow_writes`, and is refused on any other connection and on a read-only one.

Every tool call and resource read is recorded in the history with source `mcp` and the client's
`name/version` (`tobira history --mcp`). Refusals read `refused by policy: <statement>`. Stdout
carries only the protocol; logs go to the `--debug` log. Each flag replaces its `mcp:` config value,
described on the [MCP server](/docs/mcp/#configuration) page.

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `--expose <a,b>` | `mcp.expose` | Connections to serve. Nothing exposed is a usage error, as is a name that is not a connection. |
| `--allow-writes <a>` | `mcp.allow_writes` | Exposed connections `execute` may write to; each must be exposed. |
| `--max-rows <n>` | `200` | Most rows in one tool result, 1 to 1000. |
| `--send-rows <mode>` | `always` | `never` withholds row data: results carry the columns and a count. |
| `--http <host:port>` | stdio | Serve Streamable HTTP at `http://<host:port>/mcp` instead of stdio. An empty host (`:8765`) is `127.0.0.1`, never every interface; port `0` picks a free one. |
| `--insecure-bind` | off | Allow `--http` on a non-loopback address. Without it such an address exits `4` before anything listens. |
| `--allowed-host <name>` | none | Another `Host` header value to accept over HTTP, such as a name for this machine. A name alone matches the bound port; `name:port` matches exactly. Repeatable. |

### Over HTTP

```sh
tobira mcp --expose local-pg --http 127.0.0.1:8765
claude mcp add --transport http tobira http://127.0.0.1:8765/mcp --header "Authorization: Bearer <token>"
```

The address served is printed to stderr. Every request needs `Authorization: Bearer <token>`,
where the token is `mcp.token` or, without it, a random one printed once to stderr at start.
[HTTP mode](/docs/mcp/http/) covers the token, the Host and Origin checks and the limits.

### Exit codes

| Code | Meaning |
|---|---|
| `0` | The client hung up, or a signal stopped the server. |
| `1` | Usage or config error, including nothing exposed or an `mcp.token` that does not resolve. |
| `2` | The protocol streams failed, or the HTTP address could not be listened on. |
| `4` | A non-loopback `--http` address without `--insecure-bind`. |
