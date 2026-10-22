---
layout: ../../../layouts/DocsLayout.astro
title: Client setup
---

# Client setup

Each MCP client starts `tobira mcp` as a process and talks to it over stdin and stdout. This page
shows the setup for Claude Code, Claude Desktop, Cursor and VS Code. For a client that cannot start
a process, use [HTTP mode](/docs/mcp/http/).

`tobira` must be on the client's `PATH`, or use its absolute path as the command. GUI apps often do
not see your shell's `PATH`, so Claude Desktop and Cursor are safest with an absolute path. Secrets
referenced with `env:` must be set in the client's environment too.

## Claude Code

```sh
claude mcp add tobira -- tobira mcp --expose local-pg,cache
```

The `--` separates Claude Code's options from the command that starts the server. Add
`--scope project` to share it through `.mcp.json`. See the
[Claude Code MCP docs](https://code.claude.com/docs/en/mcp).

## Claude Desktop

Edit `claude_desktop_config.json` (macOS: `~/Library/Application Support/Claude/`, Windows:
`%APPDATA%\Claude\`), or open Settings, Developer, Edit Config. Then restart the app.

```json
{
  "mcpServers": {
    "tobira": {
      "command": "tobira",
      "args": ["mcp", "--expose", "local-pg,cache"]
    }
  }
}
```

See [Connect to local MCP servers](https://modelcontextprotocol.io/docs/develop/connect-local-servers).

## Cursor

Create `.cursor/mcp.json` in the project (or `~/.cursor/mcp.json` for every project):

```json
{
  "mcpServers": {
    "tobira": {
      "command": "tobira",
      "args": ["mcp", "--expose", "local-pg,cache"]
    }
  }
}
```

See the [Cursor MCP docs](https://cursor.com/docs/context/mcp).

## VS Code

Create `.vscode/mcp.json`. Its top-level key is `servers`, not `mcpServers`:

```json
{
  "servers": {
    "tobira": {
      "type": "stdio",
      "command": "tobira",
      "args": ["mcp", "--expose", "local-pg,cache"]
    }
  }
}
```

See the [VS Code MCP configuration reference](https://code.visualstudio.com/docs/agents/reference/mcp-configuration).

## Example client

[`examples/mcp-client`](https://github.com/xavidop/tobira/tree/main/examples/mcp-client) is a small
Go program that starts `tobira mcp` over stdio with [mcp-go](https://github.com/mark3labs/mcp-go),
lists the tools and calls `describe`.
