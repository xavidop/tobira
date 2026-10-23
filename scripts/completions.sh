#!/usr/bin/env bash
# Writes the bash, zsh and fish completion scripts to completions/, which
# GoReleaser ships in the release archives and installs from the Linux
# packages. Run from anywhere; it builds the CLI from cmd/tobira.
set -euo pipefail
cd "$(dirname "$0")/.."
rm -rf completions
mkdir completions
for sh in bash zsh fish; do
	(cd cmd/tobira && go run . completion "$sh") >"completions/tobira.$sh"
done
