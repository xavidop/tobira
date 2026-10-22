---
layout: ../../layouts/DocsLayout.astro
title: Install
---

# Install

tobira is a single binary with every built-in driver and the AI assistant included. Install it with
Homebrew on macOS, with `go install`, or from a release binary for Linux, macOS or Windows. Then run
`tobira doctor` to check your setup.

## Quick start

```sh
brew install xavidop/tap/tobira
tobira version
tobira doctor --no-ping
```

## Platforms

| Platform | Homebrew | Release binary | `go install` | From source |
|---|---|---|---|---|
| macOS (amd64, arm64) | yes (cask) | yes | yes | yes |
| Linux (amd64, arm64) | no | yes | yes | yes |
| Windows (amd64, arm64) | no | yes (zip) | yes | yes, with `go build` (no `make`) |

## Homebrew (macOS)

```sh
brew install xavidop/tap/tobira
tobira version
```

This installs a Homebrew cask, so it works on **macOS only**. The cask clears the quarantine
attribute after install. On Linux, use a release binary.

## Release binaries (Linux, macOS, Windows)

Download the archive for your platform from the
[GitHub releases page](https://github.com/xavidop/tobira/releases). Check it against
`checksums.txt`, unpack it, and put the `tobira` binary on your `PATH`:

```sh
sha256sum --check --ignore-missing checksums.txt   # macOS: shasum -a 256 --check --ignore-missing checksums.txt
tar xzf tobira_*_linux_amd64.tar.gz
install -m 0755 tobira ~/.local/bin/
tobira version
```

Archives are named `tobira_<version>_<os>_<arch>`: `.tar.gz` on Linux and macOS and `.zip` on
Windows, for amd64 and arm64. Each release also includes an SBOM for each archive and SLSA provenance
(`tobira.intoto.jsonl`). The binaries are not code-signed: on macOS, Gatekeeper may block a binary
you download yourself until you clear the quarantine attribute (`xattr -d com.apple.quarantine tobira`).
The Homebrew cask does this for you.

## go install

With Go 1.26 or newer:

```sh
go install github.com/xavidop/tobira/cmd/tobira@latest
tobira version
```

The binary lands in `$(go env GOPATH)/bin`; make sure that directory is on your `PATH`. A binary
built this way reports its version as `dev`; the release binaries and the Homebrew cask report the
release version.

## From source

You need Go 1.26 or newer.

```sh
git clone https://github.com/xavidop/tobira
cd tobira && make build
```

The binary is written to `cmd/tobira/tobira`.

Windows has no `make`. Build the binary with Go directly:

```sh
git clone https://github.com/xavidop/tobira
cd tobira/cmd/tobira
go build -o tobira.exe .
```

## Check the install

```sh
tobira version          # version, commit and build date
tobira doctor --no-ping # config, secret refs, drivers, plugins, AI, MCP, history; no network
```

If anything reports `fail`, see [Troubleshooting](/docs/troubleshooting/).

## Next

Follow the [Quick start](/docs/quickstart/).
