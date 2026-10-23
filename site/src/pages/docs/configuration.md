---
layout: ../../layouts/DocsLayout.astro
title: Configuration
---

# Configuration

tobira reads one YAML file with your named connections and a few optional sections: `tui:`,
`ai:`, `mcp:`, `plugins:` and `history:`. A missing file is not an error; it is an empty config. You
can edit the file by hand, or manage connections from the [terminal UI and `tobira connections`](/docs/connections/).

Any other top-level key is ignored with a warning naming its line (and the section you probably
meant, for a typo such as `histroy:`). `tobira doctor` shows the warning too.

## Where the file lives

First match wins:

1. `--config <path>`
2. the `TOBIRA_CONFIG` environment variable
3. `$XDG_CONFIG_HOME/tobira/config.yaml`
4. `~/.config/tobira/config.yaml`

A leading `~/` in the flag or the variable is expanded to your home directory.

## Connections

```yaml
connections:
  - name: local-pg
    driver: postgres
    dsn: postgres://app@localhost:5432/app?sslmode=require
    password: env:LOCAL_PG_PASSWORD
    read_only: true
    options:
      application_name: tobira-reports
  - name: notes
    dsn: sqlite:./notes.db
```

| Field | Meaning |
|---|---|
| `name` | Required, unique. What you pass to `tobira query`. |
| `driver` | Driver name, such as `sqlite`, `postgres` or `mongodb`. Optional when the DSN has a scheme that picks the driver. |
| `dsn` | The driver's native DSN. Required unless the driver needs none. See the [driver pages](/docs/drivers/). |
| `password` | A secret reference (below), never the password itself. A value that is neither a built-in reference nor a URL-form reference (`op://...`) is a load error. |
| `read_only` | `true` forces every query on this connection to be read-only. |
| `options` | Driver-specific settings, passed to the driver. Most drivers take their settings from the DSN instead and refuse any option. See [Driver options](#driver-options). |
| `color` | Sidebar colour in the [TUI](/docs/tui/): `red`, `green`, `yellow`, `orange`, `cyan`, `blue` or `magenta`. |
| `tags` | Free-form labels, shown by `tobira connections list`. |
| `ssh` | Reach the database through an SSH tunnel. A mapping with the keys below; see [SSH tunnels](/docs/connections/ssh/). |

The `ssh` keys:

| Key | Meaning |
|---|---|
| `host` | Required. The SSH server, `host` or `host:port`. |
| `port` | The SSH port when it is not in `host`; 22 by default. 1 to 65535. |
| `user` | The SSH login. Defaults to `$USER`. |
| `key` | A private key file (absolute, `~` or `$VAR`). Without it, the agent and the default keys in `~/.ssh` are tried. |
| `passphrase` | A secret reference for the key's passphrase, never the value. |
| `password` | A secret reference for SSH password authentication, tried after keys. |
| `known_hosts` | The file the server's host key is checked against (absolute, `~` or `$VAR`); `~/.ssh/known_hosts` by default. An unknown host is an error. |

A `~` is not expanded inside a DSN: use `sqlite:./notes.db` or an absolute path such as
`sqlite:/home/me/notes.db`.

Validation catches control characters in a name, driver, color, tag or DSN, an unknown key, a
duplicate name, a connection with neither a `driver` nor a DSN scheme, a field of the wrong type, a
`password` that is not a parseable reference (an empty `env:` or `file://` is refused too), and an
`ssh` block with no host, an unknown key, a bad port, a relative file path or a literal passphrase or
password. A
connection that fails validation is *broken*: tobira warns on stderr with the file, the line and the
reason, lists it as broken, and keeps every other connection working. The reason never includes a
password or any other value. See [Managing connections](/docs/connections/#broken-entries). A file
that is not valid YAML, or has a bad `tui:` section, still fails the command.

### Driver options

What each driver does with a connection's `options:` map:

| Driver | `options:` |
|---|---|
| PostgreSQL (with pgvector), CockroachDB, Redshift, Greenplum | Accepts `application_name`, `search_path` and `statement_timeout`. Any other key is refused. |
| SQLite | Reads `busy_timeout` (milliseconds, default 5000). Other keys are not used. |
| Elasticsearch, OpenSearch | Accepts `tls`, `allow_plaintext`, `ca_file`, `tls_skip_verify` and `allow_system_indices`, the same keys as the DSN. Any other key is refused. |
| Chroma | Accepts `tenant`, `database`, `tls`, `allow_plaintext` and `auth_header`. Any other key is refused. |
| Pinecone | Accepts `tls`, `allow_plaintext`, `host`, `index` and `metric`. Any other key is refused. |
| Qdrant, Milvus | Accept `tls` and `allow_plaintext`. Any other key is refused. |
| Weaviate | Accepts `scheme` and `allow_plaintext`. Any other key is refused. |
| MySQL, MariaDB, SingleStore, SQL Server, Oracle, ClickHouse, Snowflake, Vertica, libSQL, Cloudflare D1, MongoDB, Redis, Cassandra, DynamoDB, Cosmos DB | Refuse any option. Put settings in the DSN. |
| BigQuery, Cloud Firestore, InfluxDB, DuckDB (plugin) | Do not use options. Put settings in the DSN. |

A refused option makes the connection fail to open with an error that names the key. A secret never
goes in `options:`: use `password`.

## The tui section

```yaml
tui:
  theme: auto      # auto, tobira-dark or tobira-light
  keymap: default  # default or vim
```

Both keys are optional. See the [terminal UI](/docs/tui/) and the [keymaps](/docs/keymaps/).

## The ai section

```yaml
ai:
  provider: googleai          # googleai, vertexai, anthropic, openai or ollama
  api_key: env:GEMINI_API_KEY # a secret reference
  send_rows: ask              # ask, always or never
  embedder: googleai/text-embedding-005  # optional: text similarity search
```

Optional. Without it the [AI assistant](/docs/ai/) is off and tobira never contacts a model. An
unknown key under `ai:` is a load error.

| Key | Default | Meaning |
|---|---|---|
| `provider` | `googleai` | The model provider. |
| `model` | the provider's default | The model to use. |
| `api_key` | none | A secret reference like a connection's `password`, never the key itself. |
| `base_url`, `project`, `location` | none | Provider-specific settings. |
| `send_rows` | `ask` | Whether rows go to the model: `ask`, `always` or `never`. |
| `max_rows` | `50` | Rows sent per result. |
| `max_tool_calls` | `8` | Tool calls per answer. |
| `context_tokens` | `8000` | The size of the schema context. |
| `embedder` | none | `provider/model` that turns text into vectors, for [similarity search by text](/docs/tui/kind-views/). Without it, search by vector still works. |

Every key, the default model of each provider and what `send_rows` controls are on
[AI configuration](/docs/ai/configuration/). Prompt overrides live beside the config file, in
`prompts/` (see [Prompts and overrides](/docs/ai/prompts/)).

## The mcp section

```yaml
mcp:
  expose: [local-pg, cache]   # connections an MCP client may use; empty exposes nothing
  allow_writes: [cache]       # a subset of expose; the rest stay read-only
  max_rows: 200               # 1 to 1000
  send_rows: always           # always or never
  token: env:TOBIRA_MCP_TOKEN # a secret reference
```

Optional. What each key does is on the [MCP server](/docs/mcp/#configuration) page. A name that is
not a connection, and an `allow_writes` entry that is not exposed, are warnings: the file loads,
but the MCP server refuses to start until you fix them. A repeated entry is warned about and
ignored. An unknown key under `mcp:` is a load error.

## The plugins section

```yaml
plugins:
  enabled: true                # false turns plugin discovery off
  dirs: [~/my-tobira-plugins]  # replaces the default plugins directory
```

Optional. An unknown key under `plugins:` is a load error.

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `true` | `false` turns plugin discovery off. |
| `dirs` | `$XDG_CONFIG_HOME/tobira/plugins/` (else `~/.config/tobira/plugins/`) | Directories searched for `tobira-driver-<name>` executables, in order. A leading `~/` is expanded. `PATH` is still searched after them, except relative entries such as `.`. |

The first binary found for a name wins, and a built-in driver of the same name beats any plugin.
A plugin is the driver `<name>`, and DSNs with the scheme `<name>://` go to it. How tobira finds,
starts and isolates plugins is on [Plugin host](/docs/plugin-protocol/host/) and
[Security: plugin trust](/docs/security/plugins/).

## The history section

```yaml
history:
  path: ~/.local/state/tobira/history.db   # where the query history is kept
  max_entries: 10000                       # how many entries it keeps
```

Optional. A value of the wrong type and an unknown key under `history:` are load errors.

| Key | Default | Meaning |
|---|---|---|
| `path` | `$XDG_STATE_HOME/tobira/history.db`, else `~/.local/state/tobira/history.db` | Where the [query history](/docs/history/) is kept. It must be absolute or start with `~` or an environment variable (`$VAR`, `${VAR}`). A relative path is a load error. |
| `max_entries` | `10000` | How many entries the history keeps: recording a new one removes the oldest beyond that. An integer from 1 to 1,000,000; anything else is a load error. `tobira history clear` removes entries by hand. |

- A variable that is not set, or is empty, is an error: `tobira history` refuses to run and
  `tobira doctor` reports a fail. Commands that only record a query skip the history instead.
- Missing directories are created with mode 0700, and the file with mode 0600.
- When the config file cannot load at all, `tobira history` and `tobira query <dsn>` still work
  with the default path, after one warning line.

## Passwords and secrets

A connection's `password:` is a reference such as `env:LOCAL_PG_PASSWORD`, never the password
itself. See [Passwords and secrets](/docs/secrets/) for the schemes, the precedence and storing a
password in the OS keyring.
