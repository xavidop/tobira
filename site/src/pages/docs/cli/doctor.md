---
layout: ../../../layouts/DocsLayout.astro
title: tobira doctor
---

# Check your setup

`tobira doctor` checks that tobira is set up correctly and prints one line per check. Run it first
when something does not work; [Troubleshooting](/docs/troubleshooting/) explains the common
failures.

```sh
tobira doctor            # every check, with read-only pings of every connection
tobira doctor --no-ping  # no network at all: no pings, no remote secret refs
tobira doctor --online   # also ask the AI provider and start each plugin
```

## tobira doctor

```text
tobira doctor [--no-ping] [--online] [--timeout D]
```

| Flag | Default | Meaning |
|---|---|---|
| `--no-ping` | off | Do not open or ping any connection, and do not resolve a remote secret ref (any scheme other than `env`, `file`, `dotenv` and `keyring`, anywhere in a chain). Such a ref reports `skip`, "remote ref, not resolved with --no-ping". |
| `--online` | off | Also ask the AI provider whether it answers with the configured key (it lists the models and generates nothing; for `vertexai` it fetches an Application Default Credentials token), and start each plugin for its details (4 at a time, 5 seconds each). |
| `--timeout <d>` | `5s` | Timeout of each ping and each secret ref; must be positive. |

### What it checks

| Section | Checks |
|---|---|
| `config` | The file parses. Each broken entry fails; each warning warns. A connection whose DSN carries a password warns, with a hint to move it to a password reference. |
| `connections` | One row each. An unknown driver fails; a connection with an `ssh:` block fails when its DSN cannot be forwarded or its key or `known_hosts` file is missing; every other connection is opened read-only and pinged, through the tunnel when it has one. |
| `refs` | Every secret ref: connection passwords, `ai.api_key` and `mcp.token`. |
| `drivers` | The built-in drivers. |
| `plugins` | The discovered plugins with their paths, and those not used because a built-in driver or an earlier directory has the name. |
| `ai` | Provider, model, whether a key is present, the embedder config, and the key of an embedder on another provider. |
| `mcp` | Every `mcp.expose` name is a connection, `allow_writes` is a subset, and the token resolves. |
| `history` | The history file at `history.path`, else the default location: opened read-only, it reports the schema version, the size and whether tobira can write to it. It warns when the file is not writable. |
| `permissions` | The config file and the history file warn when group- or world-readable or writable. A plugins directory fails when world-writable and warns when group-writable. |

doctor only reports. It never creates, chmods or migrates a file.

### Output

Each line is `ok`, `skip`, `warn` or `fail`. `skip` is a check left out on purpose and counts as ok.
It prints a table on a terminal and JSON otherwise, as
`{"checks": [{"section", "name", "status", "detail"}]}`.

- A secret ref shows only `ok` or `fail` and its scheme (`env ref resolves`,
  `keyring ref does not resolve: not found`), never the value, its length or the rest of the ref.
- A ref whose scheme this build does not handle (such as `op://`) fails as "unknown ref scheme",
  with or without `--no-ping`.
- Every detail is redacted and kept on one line. Every secret value doctor read is masked in its
  output; a very short one may also mask unrelated text that contains it.
- Each secret ref is resolved once per run, so a `keyring:` password prompts once.

Each ping and secret ref is bounded by `--timeout`. A connection on a plugin driver starts the plugin to be
pinged. Nothing but the pings and the secret refs touches the network unless `--online`, and
plugins are never started without it.

### Exit codes

doctor has its own exit codes, and prints no `error:` line for them:

| Code | Meaning |
|---|---|
| `0` | Every check is ok or skipped. |
| `1` | Some checks warn and none fail. |
| `2` | Any check fails, or a usage error (a bad flag, an argument, a `--timeout` that is not positive), which also prints an `error:` line. |

So `1` always means warnings.
