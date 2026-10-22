---
layout: ../../../layouts/DocsLayout.astro
title: "Security: AI and MCP"
---

# Security: AI and MCP

The AI assistant sends data to the model provider you choose, and the MCP server lets an agent
query the connections you expose. This page sums up what each one can reach and send. The full
details are on [Data and safety](/docs/ai/privacy/) and the [MCP server](/docs/mcp/) pages.

## What is sent to the model

The assistant is off until you add an `ai:` section. From then on, the provider you chose receives
data, so treat that choice like any third-party service.

- **Always sent:** the schema context (names, types, keys, indexes and comments; never DDL), your
  question, the panel's conversation so far and tool results. Every string passes the connection's
  redactor first, and string values are cut at 256 characters.
- **Rows** follow `ai.send_rows`: `ask` (the default) asks before each send, `always` sends,
  `never` withholds. When rows may not be sent, a database error reaches the model only as its
  kind and native code, because errors can quote values.
- **`never` is not a hard boundary.** A withheld result still reveals its row count, truncation,
  timing and success, about one bit per call, so a model allowed many queries can learn a value
  indirectly. Keep secrets away from connections an untrusted model can query.
- **Writes.** The `query`, `sample`, `explain` and `similar` tools always run read-only, with a
  timeout and a row cap. `execute` exists only in the terminal UI, after `:ai execute on` on a
  connection that is not read-only, and asks for every statement. `tobira ask` never has it.
- **Keys.** `ai.api_key` is a secret reference, never a literal, and provider errors are redacted
  against it.
- **Prompt injection.** Data goes into prompts inside randomly named fences, and the prompts tell
  the model to treat it as data. That makes injection harder but does not prevent it. The
  read-only layers and the confirmation of `execute` do not depend on the model.
- **The Genkit Dev UI** is a developer tool that serves the assistant's flows locally; see
  [Genkit Dev UI](/docs/ai/prompts/#genkit-dev-ui) before you use it.

## What an MCP client can reach

An MCP client is an agent, and what a tool returns can contain text that tries to instruct it.
`tobira mcp` exposes nothing until `mcp.expose` or `--expose` names a connection, and answers any
other name as unknown.

- **Reads** run with the read-only guard, a timeout and `mcp.max_rows` (at most 1000).
  `mcp.send_rows` defaults to `always`, because nobody is there to answer a prompt. `never`
  withholds rows, with the same limits as the assistant's `never`.
- **Writes.** `execute` exists only when `mcp.allow_writes` is not empty, runs only on those
  connections and never on a read-only one. There is no per-call confirmation: allow writes only
  where an unattended agent may write.
- **HTTP** (`--http`) binds to loopback unless `--insecure-bind`, requires a bearer token on every
  request, checks Host and Origin against DNS rebinding, caps bodies at 4 MiB and drops idle
  sessions after 30 minutes. It is plain HTTP. See [HTTP mode](/docs/mcp/http/).
- **Audit.** Every tool call and resource read is in the history with source `mcp` and the
  client's name: `tobira history --mcp`.
