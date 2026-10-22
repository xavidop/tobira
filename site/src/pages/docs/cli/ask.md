---
layout: ../../../layouts/DocsLayout.astro
title: tobira ask
---

# Ask from the shell

`tobira ask` asks the [AI assistant](/docs/ai/) a question about a database. By default it proposes
a query and runs nothing. It needs an `ai:` section in the config file; without one it exits 1 and
says where to add it.

```sh
tobira ask pg "orders per day last week" --collection public.orders
tobira ask pg "how many users signed up today?" --answer --format json
tobira ask pg "the ten newest orders" --run --format table
echo "which tables have no primary key?" | tobira ask pg -
```

## tobira ask

```sh
tobira ask [<name|dsn>] <question|-> [--conn <name|dsn>] [--collection <ns.name|name>] [--answer | --run] [--send-rows] [--format <fmt>] [--limit <n>] [--timeout <d>]
```

The question is the last argument, or `-` to read it from stdin (at most 64 KiB). An empty question
is a usage error. With two arguments the first is the connection, a configured name or a DSN
(`tobira ask pg "orders per day"`). `--conn` does the same; giving both is a usage error. The
assistant's tools reach only that connection.

### Modes

- **Propose (default).** The assistant proposes a query and explains it. Nothing runs.
- **`--run`.** The proposed query then runs as `tobira query --read-only` would: on a connection
  opened read-only, with `--limit`, `--timeout` and `--format` as for `tobira query`, recorded in the
  history as source `cli`, with the same output and exit codes. A proposal that writes is refused
  with exit 4. The proposal and its explanation go to stderr, so stdout carries only the result.
- **`--answer`.** The assistant runs read-only queries itself, up to `ai.max_tool_calls`, and
  prints its answer followed by the queries it ran.

The command line never offers the `execute` tool; only the terminal UI does, behind a confirmation.
Queries the assistant runs are recorded in the history as source `ai`; the `ask` command itself is
not.

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `--conn <name\|dsn>` | the only configured connection | A configured name or a DSN, resolved as `tobira query` does. With none or several configured and no `--conn`, exits 1 and lists the names. |
| `--collection <ns.name\|name>` | | Focus on one collection. A bare name uses the connection's default namespace. A name that contains a dot goes in double quotes: `"a.b"."c.d"` (a doubled `""` is one quote). |
| `--answer` | off | Run read-only queries and print the answer instead of proposing a query. Cannot be combined with `--run`. |
| `--run` | off | Run the proposed query read-only and print its result. |
| `--send-rows` | off | Allow row data to be sent to the model for this run. Without it the model sees columns and counts only. Refused with exit 4 when `ai.send_rows` is `never`; has no effect when it is `always`. |
| `--format <fmt>` | `text` | `text` or `json`. With `--run`, the result's format: `table`, `json`, `ndjson` or `csv`, by default `table` on a terminal and `json` otherwise. |
| `--limit <n>` | `1000` | With `--run`: maximum rows to return, `0` for no limit. A usage error without `--run`. |
| `--timeout <d>` | `30s` | With `--run`: timeout for the query, connecting included, `0` for none. A usage error without `--run`. |

### Output

- **Text, proposal:** the query in a fenced block, the explanation, and a footer with the model,
  token counts and latency.
- **Text, `--answer`:** the answer as plain Markdown (not rendered), `Queries run:` with each query
  and its row count, and the footer.
- **JSON:** `{mode, query, explanation, usage}` for a proposal and `{mode, markdown, ran, usage}`
  for an answer. `usage` is `{model, input_tokens, output_tokens, latency_ms}`, and each `ran` entry
  has `conn`, `query`, `rows` and `error`.

Model output is untrusted. In text output, control characters are escaped and line breaks are
kept. In JSON, strings are JSON-escaped, but bidirectional and other format characters are left as
data, so a program reading the JSON must not print them to a terminal unchecked.

On a terminal, progress goes to stderr: `thinking… (<model>)` and one `▸` line per tool call. Off a
terminal nothing is printed there unless `--debug` is set.

### Exit codes

| Code | Meaning |
|---|---|
| `0` | Success. |
| `1` | Usage or config error, including AI not configured and a connection, namespace or collection the assistant cannot use. |
| `2` | The provider rejected the credentials, could not be reached, or is rate limiting. A bare `--collection` on a connection that cannot be opened also exits 2. |
| `3` | A provider or model error, or cancellation with Ctrl-C. |
| `4` | Refused by policy (`--send-rows` under `ai.send_rows: never`). |

With `--run`, the run itself exits as `tobira query` does, so a proposal that writes exits 4.
