---
layout: ../../../layouts/DocsLayout.astro
title: HTTP mode
---

# HTTP mode

`tobira mcp --http` serves Streamable HTTP instead of stdio. Use it when the MCP client cannot start
a process. It is meant for one machine: it binds to loopback and needs a bearer token on every
request.

## Quick start

Point `mcp.token` at an environment variable in the config file:

```yaml
mcp:
  expose: [local-pg]
  token: env:TOBIRA_MCP_TOKEN
```

Then start the server and add it to the client with the same token:

```sh
export TOBIRA_MCP_TOKEN=$(openssl rand -hex 32)
tobira mcp --http 127.0.0.1:8765
claude mcp add --transport http tobira http://127.0.0.1:8765/mcp --header "Authorization: Bearer $TOBIRA_MCP_TOKEN"
```

Without `mcp.token`, tobira generates a new token at each start and prints it once to stderr as
`MCP HTTP token: <hex>`, so a restart invalidates the client's copy.

## What protects the server

- **Loopback only.** An address that is not loopback exits `4` before anything listens, unless you
  pass `--insecure-bind`. An empty host (`:8765`) is `127.0.0.1`, never every interface.
- **A bearer token on every request.** `mcp.token` (a secret reference), or a random token printed
  once at start. A wrong or missing token gets `401`.
- **Host and Origin checks** against DNS rebinding. A `Host` that is not the bound address (or, on
  loopback, `localhost`, `127.0.0.1` or `[::1]` on that port) gets `403`, as does an `Origin` that
  is not loopback. Add a name for this machine with `--allowed-host`. `localhost` always listens
  on `127.0.0.1`.
- **Limits.** Bodies up to 4 MiB, and a session idle for 30 minutes is dropped; its next request
  gets `404`, so the client initializes again.

## The token

`mcp.token` must be a secret reference (`env:`, `file://`, `dotenv://` or `keyring:`). A token in a
shell history or a token file is a credential, so prefer `mcp.token: env:TOBIRA_MCP_TOKEN` over
typing it.

## Beyond loopback

tobira speaks plain HTTP. If you bind beyond loopback with `--insecure-bind`, put TLS in front of
it.

The full flag list and exit codes are in the [`tobira mcp` reference](/docs/cli/mcp/#over-http).
