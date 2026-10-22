---
layout: ../../../layouts/DocsLayout.astro
title: AI configuration
---

# AI configuration

The assistant is configured in the `ai:` section of the [config file](/docs/configuration/). This
page lists every key, the default model of each provider, where API keys come from and how to set
up the embedder for similarity search.

## Quick start

```yaml
ai:
  provider: googleai          # googleai, vertexai, anthropic, openai or ollama
  model: gemini-3.8-flash     # optional: the provider's default is used when empty
  api_key: env:GEMINI_API_KEY # a secret reference, never the key itself
  send_rows: ask              # ask, always or never
  embedder: googleai/text-embedding-005  # optional: text similarity search
```

## Keys

| Key | Default | Meaning |
|---|---|---|
| `provider` | `googleai` | One of `googleai`, `vertexai`, `anthropic`, `openai`, `ollama`. |
| `model` | the provider's default (below) | The provider's model name. |
| `api_key` | | A [secret reference](/docs/secrets/) (`env:`, `file://`, dotenv, `keyring:`). A literal key, or a value that is not a reference, is a load error. |
| `base_url` | | An `http://` or `https://` URL, without credentials: an OpenAI-compatible server for `openai`, the server address for `ollama`. |
| `project` | | The Google Cloud project, for `vertexai`. |
| `location` | `us-central1` | The Google Cloud region, for `vertexai`. |
| `send_rows` | `ask` | Whether row data may be sent to the model: `ask`, `always` or `never`. See [send_rows](/docs/ai/privacy/#send_rows). `send_sample_rows: true\|false` is an alias for `always\|never`; set one or the other, not both. |
| `max_rows` | `50` | Rows in one tool result, 1 to 200. |
| `max_tool_calls` | `8` | Tool calls in one answer, 1 to 64. When the cap is reached the model is told to answer with what it has. |
| `context_tokens` | `8000` | Approximate size of the schema context sent with each question, 1000 to 1000000. |
| `embedder` | | The embedding model for text similarity search: see [Embedder](#embedder). |

An unknown key under `ai:` is a load error that suggests the closest key. No message ever quotes
the value of a key.

### Default models

| Provider | Default model |
|---|---|
| `googleai` | `gemini-3.8-flash` |
| `vertexai` | `gemini-3.8-flash` |
| `anthropic` | `claude-sonnet-5-5` |
| `openai` | `gpt-5` |
| `ollama` | `llama3.3` |

Set `model` to use another one. The panel header and the `tobira ask` footer show the model in use.

## Keys and credentials

- **`googleai`, `anthropic`, `openai`.** The key comes from `api_key` when it is set. Otherwise
  tobira reads the provider's environment variable: `GEMINI_API_KEY` and then `GOOGLE_API_KEY` for
  `googleai`, `ANTHROPIC_API_KEY` for `anthropic`, `OPENAI_API_KEY` for `openai`. With neither,
  asking fails with an authentication error that names the variable.
- **`openai` with `base_url`.** For an OpenAI-compatible server (a local gateway, for example) a key
  is optional. With none configured, tobira sends a placeholder key, which such servers usually
  ignore.
- **`vertexai`.** Uses Application Default Credentials (`gcloud auth application-default login`,
  or `GOOGLE_APPLICATION_CREDENTIALS`) with `project` and `location`. It takes no API key; an
  `api_key` is ignored. Credentials are looked up on first use, not at startup.
- **`ollama`.** Talks to `http://localhost:11434`, or to `base_url`. It takes no key; an `api_key`
  is ignored. Use a model that supports tool calling, because the answer mode needs it.

Provider error messages are redacted: they never contain the key, a password or a secret
reference.

## Embedder

`embedder` names the embedding model that turns text into vectors, for similarity search by text on
vector databases: `:similar <text>` in the terminal UI, and the `similar` tool of the assistant and
the [MCP server](/docs/mcp/tools/).

- Write it as `provider/model`, for example `googleai/text-embedding-005`,
  `openai/text-embedding-3-small` or `ollama/nomic-embed-text`, or as a model name of `provider`.
  `anthropic` has no embedding models.
- `api_key` and `base_url` apply only when the embedder uses `provider`. Another provider reads its
  key from its environment variable.
- The key is read on the first text search, not at start.
- Without an embedder, searching by text says "text similarity needs ai.embedder". Searching with a
  vector still works.
