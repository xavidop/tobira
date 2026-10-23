---
layout: ../../layouts/DocsLayout.astro
title: Install
---

# Install

tobira is a single binary with every built-in driver and the AI assistant included. Install it with
Homebrew on macOS, Scoop on Windows, a `.deb`, `.rpm` or `.apk` package on Linux, or from a release
archive for any of the three. Then run `tobira doctor` to check your setup.

## Quick start

```sh
brew install xavidop/tap/tobira
tobira version
tobira doctor --no-ping
```

## Platforms

| Platform | Package manager | Package | Release archive | From source |
|---|---|---|---|---|
| macOS (amd64, arm64) | Homebrew (cask) | no | yes | yes |
| Linux (amd64, arm64) | no | `.deb`, `.rpm`, `.apk` | yes | yes |
| Windows (amd64, arm64) | Scoop | no | yes (zip) | yes, with `go build` (no `make`) |

`go install` of the CLI module does not work: it depends on the other modules of its checkout,
which `go install` cannot resolve. Build [from source](#from-source) instead.

## Homebrew (macOS)

```sh
brew install xavidop/tap/tobira
tobira version
```

This installs a Homebrew cask, so it works on **macOS only**. The cask clears the quarantine
attribute after install. On Linux, use a package or a release archive.

## Scoop (Windows)

```powershell
scoop bucket add xavidop https://github.com/xavidop/scoop-bucket
scoop install tobira
tobira version
```

Scoop installs the Windows zip from the release and keeps it up to date with `scoop update tobira`.

## Linux packages

Each release attaches a `.deb`, an `.rpm` and an `.apk` for amd64 and arm64, named
`tobira_<version>_linux_<arch>.<format>`. There is no package repository: download the file for your
distribution from the [GitHub releases page](https://github.com/xavidop/tobira/releases) and
install it. For the latest release on amd64:

```sh
v=$(curl -fsSL https://api.github.com/repos/xavidop/tobira/releases/latest | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p')
curl -fsSLO "https://github.com/xavidop/tobira/releases/download/v$v/tobira_${v}_linux_amd64.deb"
sudo dpkg -i "tobira_${v}_linux_amd64.deb"      # Debian, Ubuntu
# sudo rpm -i "tobira_${v}_linux_amd64.rpm"     # Fedora, RHEL, openSUSE
# sudo apk add --allow-untrusted "tobira_${v}_linux_amd64.apk"   # Alpine
tobira version
```

The package puts `tobira` in `/usr/bin` and installs the bash, zsh and fish completions. The
packages are not signed, hence `--allow-untrusted` on Alpine; check the file against
`checksums.txt` from the same release if you want to verify it.

## Release archives (Linux, macOS, Windows)

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
Windows, for amd64 and arm64. Each holds the binary, the license, the README and the shell
completions under `completions/`. Each release also includes an SBOM for each archive and SLSA
provenance (`tobira.intoto.jsonl`). The binaries are not code-signed: on macOS, Gatekeeper may block
a binary you download yourself until you clear the quarantine attribute
(`xattr -d com.apple.quarantine tobira`). The Homebrew cask does this for you.

## From source

You need Go 1.26 or newer.

```sh
git clone https://github.com/xavidop/tobira
cd tobira && make build
```

The binary is written to `cmd/tobira/tobira`. A binary built this way reports its version as `dev`;
the release binaries and the packages report the release version.

Windows has no `make`. Build the binary with Go directly:

```sh
git clone https://github.com/xavidop/tobira
cd tobira/cmd/tobira
go build -o tobira.exe .
```

## Shell completion

`tobira completion <shell>` prints a completion script for bash, zsh, fish or PowerShell, so the
shell completes commands, flags and their values. The Linux packages install it for bash, zsh and
fish; after any other install, load it yourself:

```sh
# bash: once per machine (needs the bash-completion package)
tobira completion bash > /etc/bash_completion.d/tobira          # Linux
tobira completion bash > $(brew --prefix)/etc/bash_completion.d/tobira   # macOS, Homebrew bash-completion

# zsh: once, then start a new shell (`compinit` must be enabled in ~/.zshrc)
tobira completion zsh > "${fpath[1]}/_tobira"                  # Linux
tobira completion zsh > $(brew --prefix)/share/zsh/site-functions/_tobira   # macOS

# fish
tobira completion fish > ~/.config/fish/completions/tobira.fish
```

```powershell
# PowerShell: add to your profile
tobira completion powershell | Out-String | Invoke-Expression
```

`tobira completion <shell> --help` prints the same steps for that shell. To try it in the current
shell only, `source <(tobira completion bash)` or `source <(tobira completion zsh)`.

## Check the install

```sh
tobira version          # version, commit and build date
tobira doctor --no-ping # config, secret refs, drivers, plugins, AI, MCP, history; no network
```

If anything reports `fail`, see [Troubleshooting](/docs/troubleshooting/).

## Next

Follow the [Quick start](/docs/quickstart/).
