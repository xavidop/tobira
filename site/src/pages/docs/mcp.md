---
layout: ../../layouts/DocsLayout.astro
title: MCP server
---

# MCP server

`tobira mcp` turns tobira into a [Model Context Protocol](https://modelcontextprotocol.io/) server,
so an MCP client such as Claude Code, Claude Desktop, Cursor or VS Code can look at your databases
and run read-only queries on them. It uses the same tools as the [AI assistant](/docs/ai/), and
every call is recorded in the [history](/docs/history/).

Nothing is exposed by default. A client can see a connection only if you name it, with `--expose`
or in the `mcp:` section of the [config file](/docs/configuration/#the-mcp-section). Any other name
is answered exactly as if it did not exist.

An MCP client is an agent: it decides on its own what to ask, and what it reads goes to the model
behind it. Expose only the connections it should see, and prefer connections that are
`read_only: true`.

## Quick start

```sh
tobira connections add --name local-pg --driver postgres --dsn 'postgres://app@localhost/app' --read-only
claude mcp add tobira -- tobira mcp --expose local-pg
```

Then ask Claude Code something about the database. To check the server by hand, run
`tobira mcp --expose local-pg` and type nothing: it waits for a client on stdin and exits when stdin
closes.

## In this section

| Page | Covers |
|---|---|
| [Client setup](/docs/mcp/clients/) | Claude Code, Claude Desktop, Cursor and VS Code |
| [Tools and resources](/docs/mcp/tools/) | The tools, the schema resources, `send_rows`, writes and the call history |
| [HTTP mode](/docs/mcp/http/) | Serving over Streamable HTTP with a bearer token |
| [`tobira mcp`](/docs/cli/mcp/) | Every flag and exit code |

## Configuration

Each flag replaces its `mcp:` config value.

```yaml
mcp:
  expose: [local-pg, cache]   # connections an MCP client may use; empty exposes nothing
  allow_writes: [cache]       # a subset of expose; the rest stay read-only
  max_rows: 200               # 1 to 1000
  send_rows: always           # always or never
  token: env:TOBIRA_MCP_TOKEN # HTTP only: a secret reference
```

| Config key | Flag | Default |
|---|---|---|
| `expose` | `--expose a,b` | none, so `tobira mcp` exits `1` until you set one |
| `allow_writes` | `--allow-writes a` | none: every exposed connection is read-only |
| `max_rows` | `--max-rows n` | `200` (1 to 1000) |
| `send_rows` | `--send-rows always\|never` | `always` |
| `token` | none | a random token per start (HTTP only) |

- A name in `expose` or `allow_writes` that is not a connection, and an `allow_writes` entry that
  is not exposed, are reported as warnings when the file loads. `tobira mcp` then refuses to start
  until they are fixed, and the error names the entry's position in the file.
- A repeated entry is warned about and ignored: it never stops the server.
- `mcp.token` must be a secret reference (`env:`, `file://`, `dotenv://` or `keyring:`); a literal
  token is a load error.

## Security

- The exposed set is the policy. Everything the tools can reach on those connections can reach the
  client's model.
- Prefer `read_only: true` connections and a database user that can only read. The read-only guard
  refuses anything it cannot recognise as a read, but a database role is the boundary that does not depend on tobira.
- Tool results are data from your database. A row can contain text that tries to instruct the
  model. The only defence that does not depend on the model is what the connection is allowed to
  do.
- Schemas are served without DDL, and error text is redacted against the connection's secrets.
- `execute` exists only for connections in `allow_writes`, and there is no per-call confirmation.

See [Security](/docs/security/ai-and-mcp/) for the full picture.
