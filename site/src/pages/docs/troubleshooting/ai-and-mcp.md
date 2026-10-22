---
layout: ../../../layouts/DocsLayout.astro
title: "Troubleshooting: AI and MCP"
---

# Troubleshooting: AI and MCP

Fixes for the AI assistant (keys, models, providers) and for MCP clients. `tobira doctor` checks the
`ai:` and `mcp:` sections, and `tobira doctor --online` also checks that the provider accepts the
key.

## The AI assistant

- **"AI is not configured".** There is no `ai:` section. Add one; see [AI configuration](/docs/ai/configuration/).
- **"ai provider authentication failed".** No key was found, or the provider rejected it. Set
  `ai.api_key` to a reference such as `env:GEMINI_API_KEY`, or export the provider's variable
  (the error names it). For `vertexai`, check your Application Default Credentials.
- **Model not found.** The provider reports the model as unknown (exit 3 from `tobira ask`). Check
  the spelling of `ai.model`, or leave it empty for the default. For `ollama`, pull the model
  first (`ollama pull llama3.3`).
- **Rate limit.** The provider is refusing for quota (exit 2). Wait, switch model, or raise the
  quota.
- **Provider unreachable.** For `ollama` or a `base_url`, check the server is running and the URL;
  for hosted providers, check the network (exit 2).
- **Ollama model without tool support.** tobira does not check whether an `ollama` model supports
  tool calling, so a model without it fails with a provider error (exit 3) as soon as the assistant
  calls a tool. Choose a model that supports tools.
- **The assistant only sees schema, no rows.** That is `send_rows`. Say yes at the prompt, set
  `always`, or pass `--send-rows`.
- **No queries are run on my connection.** Its driver has no read-only guard (SingleStore, for
  example), so only the schema tools are offered.
- **`no key: set ai.api_key (a secret ref) or GEMINI_API_KEY`** (or `ANTHROPIC_API_KEY`,
  `OPENAI_API_KEY`) in doctor's `ai` section. Set `ai.api_key` to a reference such as
  `env:GEMINI_API_KEY`, or export the provider's variable. A literal key in the config is a load
  error. `vertexai` uses Application Default Credentials and `ollama` needs no key.
- **Check a key without asking anything.** `tobira doctor --online` lists the provider's models,
  without generating anything; for `vertexai` it fetches an ADC token instead.
- **Text similarity says "text similarity needs ai.embedder".** Set `ai.embedder`, for example
  `googleai/text-embedding-005`. Its provider needs its own key, which doctor checks in the row
  named `embedder api_key` of the `ai` section.

## MCP clients

- **The client sees no connections.** Nothing is exposed until `mcp.expose` or `--expose` names it,
  and doctor's `mcp` section fails an exposed name that is not a valid connection.
- **401 over HTTP.** Every request needs `Authorization: Bearer <token>`. Without `mcp.token`, tobira
  generates a token at each start and prints it once to stderr, so a restart invalidates the
  client's copy; set `mcp.token: env:TOBIRA_MCP_TOKEN` to keep it stable. `mcp.token` must be a
  reference, never a literal.
- **403 over HTTP.** The Host or Origin check refused the request: connect to the address tobira
  bound (`127.0.0.1:8765`, `localhost:8765`), or add a name with `--allowed-host`.
- **`--http` on a non-loopback address exits 4.** Binding beyond loopback needs `--insecure-bind`;
  put TLS in front of it, since tobira speaks plain HTTP.
- **The client cannot start `tobira`.** MCP clients start the binary with their own `PATH`; use the
  absolute path to `tobira` in the client's config. Secrets referenced with `env:` must be set in
  that config too.
- `tobira history --mcp` shows every call a client made, refusals included. See the
  [MCP server](/docs/mcp/) page.
