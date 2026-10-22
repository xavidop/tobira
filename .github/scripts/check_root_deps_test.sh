#!/usr/bin/env bash
# Tests check_root_deps.sh: it must reject a module that depends on a database
# SDK directly or transitively, and accept the repo root.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="$root/.github/scripts/check_root_deps.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

expect_exit() { # expect_exit <want> <moddir> <label>
	local want="$1" dir="$2" label="$3" got=0
	bash "$script" "$dir" >"$tmp/out.txt" 2>&1 || got=$?
	[ "$got" -eq "$want" ] || { cat "$tmp/out.txt" >&2; fail "$label: want exit $want, got $got"; }
	echo "ok: $label (exit $got)"
}

# Direct dependency on a database driver.
mkdir -p "$tmp/direct"
cat >"$tmp/direct/go.mod" <<'MOD'
module example.com/direct

go 1.26.9
MOD
cat >"$tmp/direct/main.go" <<'GO'
package main

import _ "github.com/jackc/pgx/v5"

func main() {}
GO
(cd "$tmp/direct" && GOWORK=off GOFLAGS=-mod=mod go get github.com/jackc/pgx/v5@latest >/dev/null 2>&1)

# Transitive dependency: a requires b (local replace), b requires modernc.org/sqlite.
mkdir -p "$tmp/trans/a" "$tmp/trans/b"
cat >"$tmp/trans/b/go.mod" <<'MOD'
module example.com/b

go 1.26.9
MOD
cat >"$tmp/trans/b/b.go" <<'GO'
package b

import _ "modernc.org/sqlite"
GO
(cd "$tmp/trans/b" && GOWORK=off GOFLAGS=-mod=mod go get modernc.org/sqlite@latest >/dev/null 2>&1)
cat >"$tmp/trans/a/go.mod" <<'MOD'
module example.com/a

go 1.26.9

require example.com/b v0.0.0

replace example.com/b => ../b
MOD
cat >"$tmp/trans/a/a.go" <<'GO'
package a

import _ "example.com/b"
GO
(cd "$tmp/trans/a" && GOWORK=off GOFLAGS=-mod=mod go mod tidy >/dev/null 2>&1)

expect_exit 1 "$tmp/direct" "direct pgx dependency is rejected"
expect_exit 1 "$tmp/trans/a" "transitive sqlite dependency is rejected"
expect_exit 0 "$root" "repo root is clean"
echo "PASS"
