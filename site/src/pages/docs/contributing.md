---
layout: ../../layouts/DocsLayout.astro
title: Contributing
---

# Contributing

Contributions are welcome, new drivers especially. This page is the short version. The full guide
is [CONTRIBUTING.md](https://github.com/xavidop/tobira/blob/main/CONTRIBUTING.md) in the
repository.

## Before you start

- Read the [Code of Conduct](https://github.com/xavidop/tobira/blob/main/CODE_OF_CONDUCT.md).
- Open an issue before a large change, and before you start a new driver, so we can agree on the
  scope first.
- Never commit real credentials: not in code, tests, fixtures or issues.
- Report security problems privately; see [Security](/docs/security/#reporting-a-vulnerability).

## Build and test

tobira is a multi-module repository tied together by `go.work`. You need Go 1.26 or newer, and
Node 22 (through nvm) for the docs site.

```sh
make build          # build cmd/tobira/tobira
make all            # fmt, vet, check-deps, check-claims, test and lint, every module
make check-claims   # check the README and docs against the code
make site-linkcheck # build this site and fail on a broken internal link
make demo           # record the README's demo GIF with vhs
```

## Integration tests

`make all` runs the unit tests, which need no database. Integration tests run with
`go test -tags integration` in a driver module and start real databases with testcontainers, so
they need Docker.

- Without a Docker daemon they stop at once with an error such as
  `start postgres container: Cannot connect to the Docker daemon`. Start Docker Desktop, or another
  Docker-compatible runtime such as Colima or Podman. With a non-default socket, point
  `DOCKER_HOST` at it (for Colima, `unix://$HOME/.colima/default/docker.sock`).
- Or leave them to CI, which runs the integration suite of every driver on Linux.
- SQLite and Cloudflare D1 need no container: all their tests run in `make all`. The DuckDB
  plugin's tests use an embedded DuckDB.
- Some suites use large images: Snowflake builds a fakesnow image, pinecone-local is amd64 only and
  runs under emulation on Apple silicon, and Oracle's image needs about 2.3 GB of disk.

## Keep the docs in step

A feature appears in the README or on this site only once it exists and is tested.
`make check-claims`, part of `make all`, checks the docs against the code: the CLI reference
against the command tree, the drivers matrix against the registered drivers and their
capabilities, the driver count, the MCP tools and the `ai:` defaults. When you add a command, a
flag or a capability, update the [CLI reference](/docs/cli/), the README and the agent skill in the
same commit.

## Commits

Use [Conventional Commits](https://www.conventionalcommits.org/): `fix:` is a patch, `feat:` a minor
and `feat!:` or `BREAKING CHANGE:` a major release. Scopes are `driver`, `drivertest`, `cli`,
`site` and the plain driver name (`postgres`, `sqlite`, …).

## Write a driver

Start from [Write a driver](/docs/write-a-driver/) and `examples/custom-driver-module`, or
`examples/custom-driver-plugin` for an external plugin. Every driver must pass the `drivertest`
conformance kit with `Strict: true`, and a new database is a new `drivers/<name>` module: the root
module never depends on a database SDK.
