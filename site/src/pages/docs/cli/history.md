---
layout: ../../../layouts/DocsLayout.astro
title: tobira history
---

# List the query history

`tobira history` lists the queries that `tobira query`, the terminal UI's editor, the AI
assistant's tools and MCP clients ran, newest first. What is stored, where, for how long and how
credentials are masked is on the [Query history](/docs/history/) page.

```sh
tobira history
tobira history --conn local-pg --search "from orders" --limit 20
tobira history --mcp --format json
```

## tobira history

```sh
tobira history [--conn <name>] [--source cli|tui|ai|mcp] [--mcp] [--search <text>] [--limit <n>] [--format table|json|ndjson|csv]
```

Each entry has `at`, `conn`, `source` (`cli`, `tui`, `ai` or `mcp`), `ok`, `rows`, `ms` and
`query`. `json` and `ndjson` also carry `client` (null unless the call came from an MCP client), and
`table` and `csv` add a `client` column for an MCP listing.

| Flag | Default | Meaning |
|---|---|---|
| `--conn <name>` | all | Only entries of this connection. |
| `--source <src>` | all | Only entries from one source: `cli`, `tui`, `ai` or `mcp`. Any other value is a usage error. |
| `--mcp` | | Only calls made by MCP clients, with the client's name. The same as `--source mcp`. |
| `--search <text>` | | Only entries whose query contains the text, ignoring case (non-ASCII text too). |
| `--limit <n>` | `100` | Maximum entries; `0` lists every retained entry (up to 10,000). A negative value is a usage error. |
| `--format <fmt>` | `table` on a terminal, else `json` | Same formats as `tobira query`. |

With no history yet it prints nothing (`[]` for `json`) and exits 0.
