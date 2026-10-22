---
layout: ../../layouts/DocsLayout.astro
title: AI assistant
---

# AI assistant

tobira can answer questions about your database in plain language. It can propose a query, answer
a question by running read-only queries itself, explain a result tab, and fix a query that failed.
Use it in the [terminal UI](/docs/tui/) (the AI panel, key `a`) or from the shell with
[`tobira ask`](/docs/cli/ask/). It works with Gemini, Vertex AI, Claude, OpenAI (or any
OpenAI-compatible server) and Ollama.

The assistant never writes to your database on its own. Its query tools are read-only. The one tool
that can write, `execute`, exists only in the terminal UI, only after you turn it on for a
connection with `:ai execute on`, and asks you to confirm every statement.

## Quick start

Add an `ai:` section to the [config file](/docs/configuration/). Without one, the assistant is off
and tobira never contacts a model.

```yaml
ai:
  provider: googleai          # googleai, vertexai, anthropic, openai or ollama
  api_key: env:GEMINI_API_KEY # a secret reference, never the key itself
  send_rows: ask              # ask, always or never
```

Then ask a question:

```sh
tobira ask demo "how many pets have a note?" --collection pets           # propose a query
tobira ask demo "how many pets have a note?" --answer                     # run read-only queries and answer
```

In the terminal UI, press `a` to open the panel, type a question and press `enter`.

## In this section

| Page | Covers |
|---|---|
| [Configuration](/docs/ai/configuration/) | Providers, every `ai:` key, default models, API keys and the embedder |
| [Using the assistant](/docs/ai/using/) | The terminal UI panel, `tobira ask` and similarity search |
| [Data and safety](/docs/ai/privacy/) | What is sent to the model, `send_rows`, the read-only layers and prompt injection |
| [Prompts](/docs/ai/prompts/) | Overriding the built-in prompts, and the Genkit Dev UI |

Problems with keys, models or providers are covered in
[Troubleshooting](/docs/troubleshooting/ai-and-mcp/).

## How it stays safe

- **Read-only queries.** The assistant's query tools run only on connections whose driver has the
  read-only guard, always with the read-only flag, a 30 second timeout and a row cap.
- **One connection.** The tools reach only the connection of the conversation and refuse any
  other name.
- **You decide what leaves.** `send_rows` decides whether row data may reach the model. The
  default, `ask`, asks you each time.
- **No secrets in prompts.** DDL is never sent, every string is redacted against the connection's
  secrets, and the API key is a secret reference.

The details are on [Data and safety](/docs/ai/privacy/).
