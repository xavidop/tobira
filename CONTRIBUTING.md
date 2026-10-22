# Contributing to tobira

Thanks for your interest. Contributions are welcome, especially new drivers.

## Ground rules

- Be kind. See the [Code of Conduct](CODE_OF_CONDUCT.md).
- **Never commit real credentials**: not in code, tests, fixtures or issue reports.
- Open an issue before a large change, so we agree on scope before you build it.
- Report security problems privately, as described in [SECURITY.md](SECURITY.md).

## Build and test

You need **Go 1.26 or newer**. A plain clone builds on its own.

```bash
git clone https://github.com/xavidop/tobira
cd tobira
make build       # build every module; the binary is cmd/tobira/tobira
make all         # what CI runs: fmt check, vet, check-deps, check-claims, tests and lint
```

Other targets:

```bash
make test           # test every module
make lint           # golangci-lint across modules
make check-claims   # the README and docs against the code: commands, flags, drivers and counts
make site-build     # build the docs site (Node 22 via nvm)
make site-linkcheck # build the site and fail on a broken internal link
make demo           # record site/public/demo.gif with vhs (needs vhs, ttyd and ffmpeg)
make proto          # regenerate the plugin protocol's Go code after editing the .proto
make proto-check    # fail if the committed generated code is stale
```

Every module is built with the workspace off (`GOWORK=off`), as the Makefile does, so each
`go.mod` stays correct on its own.

Integration tests that need a real database run under `-tags integration` with testcontainers, so
they need Docker. `make all` does not run them. See
[Troubleshooting](https://xavidop.github.io/tobira/docs/troubleshooting/) for running without
Docker.

## Repository layout

This is a multi-module repository tied together by a `go.work` file:

- `/` (module `github.com/xavidop/tobira`): the core. It holds the `driver` contract, the
  `drivertest` conformance kit, `api` (the stable surface for plugins and programs that embed
  tobira), `driverutil` (shared SQL scanning, completion and network rules), the plugin host and
  protocol under `plugin/`, and the CLI, TUI, config, secrets and history under `internal/`. It
  must never require a database SDK: `make check-deps` fails the build if it does.
- `cmd/tobira`: the `tobira` binary, its own module. It links every driver and the AI module.
- `ai`: the AI assistant and the MCP server, built on [Genkit](https://genkit.dev). It is its own
  module so the core needs no model SDK.
- `drivers/<name>`: one module per database driver (`bigquery`, `cassandra`, `chroma`,
  `clickhouse`, `cosmos`, `d1`, `duckdb`, `dynamodb`, `elasticsearch`, `firestore`, `influxdb`,
  `libsql`, `milvus`, `mongodb`, `mssql`, `mysql`, `oracle`, `pinecone`, `postgres`, `qdrant`,
  `redis`, `snowflake`, `sqlite`, `vertica`, `weaviate`), so a database SDK never reaches the core.
  `duckdb` builds the external plugin `tobira-driver-duckdb` (with cgo).
- `plugin/sdk`: the Go SDK for external driver plugins, its own module.
- `examples/custom-driver-module`: a small driver that passes the conformance kit.
- `examples/custom-driver-plugin`: the same idea as an external plugin (`memdemo`).
- `examples/embedding`: tobira's driver layer used as a library in another Go program.
- `examples/mcp-client`: a small Go MCP client that starts `tobira mcp` and calls `describe`.
- `skills/tobira`: the agent skill.
- `site/`: the Astro documentation site.

## Writing a driver

Read [Write a driver](https://xavidop.github.io/tobira/docs/write-a-driver/) and start from
[`examples/custom-driver-module`](examples/custom-driver-module). The SQL drivers
([`drivers/sqlite`](drivers/sqlite), [`drivers/postgres`](drivers/postgres),
[`drivers/mysql`](drivers/mysql)) show a full implementation with browse, explain, editing,
transactions and completion. [`drivers/mongodb`](drivers/mongodb), [`drivers/redis`](drivers/redis)
and [`drivers/qdrant`](drivers/qdrant) show the same contract on document, key-value and vector
stores.

Every driver must pass the `drivertest` conformance kit with `Strict: true`. Open an issue before
you start a new driver. A driver that should not live in this repository can be an external plugin
instead: start from [`examples/custom-driver-plugin`](examples/custom-driver-plugin).

The plugin protocol is `plugin/proto/tobira/plugin/v1/driver.proto`. After you change it, run
`make proto` and commit the regenerated `*.pb.go` files. The tools are pinned and run with
`go run`, so you do not need to install anything. Keep field numbers stable: never renumber or
reuse one, and mark a removed field `reserved`.

### Checklist for a built-in driver

A driver contributed to this repository meets this bar:

- **Module.** One module under `drivers/<name>` with `replace github.com/xavidop/tobira => ../..`.
  The root module never requires a database SDK (`make check-deps`).
- **Registration.** A blank import in `cmd/tobira`, unless the driver needs cgo, in which case it
  ships as an [external plugin](https://xavidop.github.io/tobira/docs/write-a-driver/plugin/).
- **Kind.** One of the existing kinds. Cassandra and ScyllaDB are `KindWideColumn`; Elasticsearch
  and OpenSearch are `KindSearch`. Both set their [kind fixture](https://xavidop.github.io/tobira/docs/write-a-driver/conformance/#kind-fixtures).
- **Query language.** The database's native one (a SQL dialect, CQL, Query DSL JSON, PartiQL,
  Cosmos SQL, GoogleSQL). An API without one gets a small, documented JSON request format, as the
  MongoDB and Qdrant drivers have.
- **Read-only guard.** It declares `CapReadOnly` and refuses whatever it cannot prove is a read. It uses the protocol's
  read-only mode where one exists, otherwise an allow-list of native reads. A statement it cannot
  classify is refused. List the compound forms it refuses in `Fixture.CompoundWrites`.
- **Errors.** Every error goes through `driver.Error` and `RedactMessage`. The DSN forms are
  documented, and passwords, keys and tokens come from secret references.
- **Capabilities.** Browser, Explainer, Completer and Mutator where the database supports them,
  with mutations keyed by primary key, document id or key. Transactor only for real transactions.
  VectorSearcher for vector search. Watcher only for a native change feed.
- **Tests.** `drivertest` Strict with the per-kind fixture, with `TypeOverrides` only for
  [declared storage limits](https://xavidop.github.io/tobira/docs/write-a-driver/conformance/#declared-storage-limits). The integration suite runs behind
  `-tags integration` against a container at an exact pinned image tag, with testcontainers'
  random ports and `t.Cleanup` Terminate. A suite that cannot run against a real server or emulator
  says so in the README and the docs page.
- **Docs.** The driver README, `site/src/pages/docs/drivers/<name>.md`, its row in the drivers
  matrix and the sidebar.
- **CI.** One integration job per driver, plus its dependabot and `go.work` entries.

## Working on the AI assistant

The assistant's prompts are Dotprompt files in [`ai/prompts/`](ai/prompts), embedded in the
binary. A file with the same name in `$XDG_CONFIG_HOME/tobira/prompts/` (by default
`~/.config/tobira/prompts/`) replaces the built-in prompt, so you can try a change without
rebuilding.

To run the flows (`askFlow`, `answerFlow`, `explainResultsFlow`, `fixErrorFlow`) in the Genkit Dev
UI with your normal config and its `ai:` section:

```bash
genkit start -- tobira ai dev
```

`tobira ai dev` is a hidden command. It registers every flow in Genkit dev mode and runs until you
stop it. Each flow takes its scope as input, for example
`{"scope": {"conn": "local"}, "question": "..."}`. Nobody can answer a row-consent prompt in the
Dev UI, so `send_rows: ask` sends no rows, and `execute` is never offered.

Dev mode exposes more than the TUI does, so use it only with test connections, `send_rows: never`
or a throwaway config:

- While it runs, any local process can call the flows on `127.0.0.1`. A call runs read queries on
  every configured connection and sends rows to the provider as `send_rows` allows.
- Genkit may store traces of each run, with prompts, rows and answers, under `.genkit/` in the
  working directory. Delete it when you are done, and never commit it.

Tests never call a real provider. They use the scripted fake model in [`ai/aitest`](ai/aitest). A
binary built with `-tags aifake` also accepts `ai.provider: fake` and answers from the JSON script
named by `$TOBIRA_AI_FAKE_SCRIPT`, for end-to-end tests. A normal build has no fake provider.

## Commits and pull requests

- Keep pull requests focused, and reference the issue they resolve.
- Use [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`,
  `feat(postgres):` and so on). Releases and the changelog are generated from them: `fix:` makes a
  patch release, `feat:` a minor one and `feat!:` or `BREAKING CHANGE:` a major one.
- Scopes are `driver`, `drivertest`, `cli`, `site` and the plain driver name, for example
  `postgres` or `sqlite`.
- Fill out the pull request template.
- CI must be green before review.
