#!/usr/bin/env bash
# Fails when the user-facing docs disagree with the code or with each other:
#   1. no user-facing doc promises a feature for a later milestone or still
#      calls driver plugins planned, and the root docs carry no milestone,
#      status or release-process wording;
#   2. the plugin docs name the required --sha256;
#   3. CHANGELOG.md opens with the changelogTitle that semantic-release keeps;
#   4. the CLI reference, the README and the skill name every command and flag
#      the cobra tree has (and no command that does not exist);
#   5. the drivers tables list exactly the drivers the binary registers, with
#      the capabilities each really has, and every driver count matches;
#   6. the Claude Code one-liner in the README and the MCP page uses flags
#      that `tobira mcp` has;
#   7. the AI page names the default model of each provider and the defaults
#      of the ai: keys that the code has;
#   8. the README has its fixed shape (header, then Why, Install, Quick start,
#      Features, Drivers, AI & MCP, CLI, Agent skill, Documentation,
#      Contributing, License), its Install section matches .goreleaser.yaml,
#      its CLI block matches the cobra tree, its links resolve, and its AI &
#      MCP section matches the providers, tools and mcp: limits of the code;
#      CONTRIBUTING's repository layout matches go.work.
# Checks 4 to 8 are Go tests, so they read the real command tree, registry and
# provider table.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"

root_docs=(README.md SECURITY.md CONTRIBUTING.md CHANGELOG.md skills/tobira/SKILL.md examples/*/README.md)

# 1. Nothing user-facing may promise a milestone or call plugins planned.
stale="$(grep -rnIiE 'coming in M[0-9]|arrives? (in|with) (M[0-9]|a later)|later milestone|planned for M[0-9]|planned \(M[0-9]\)|\(M[0-9]\)( \||$)|polish milestone|M[0-9] is next|lands in M[0-9]|until M[0-9]|plugins? \(planned\)|plugin route is planned|(out-of-process )?plugins?[^.]*(is|are) planned|later out-of-process plugin' \
	README.md SECURITY.md CONTRIBUTING.md CHANGELOG.md skills examples site/src --include='*.md' --include='*.astro' --include='*.ts' || true)"
if [ -n "$stale" ]; then
	echo "check-docs-claims: these lines promise a milestone or call plugins planned:" >&2
	printf '%s\n' "$stale" >&2
	exit 1
fi
internal="$(grep -nIiE '(^|[^a-z0-9])M[0-9]([^a-z0-9]|$)|pending release|release blockers?|docs/superpowers|milestone' "${root_docs[@]}" || true)"
if [ -n "$internal" ]; then
	echo "check-docs-claims: the root docs carry status, milestone or release-process wording:" >&2
	printf '%s\n' "$internal" >&2
	exit 1
fi
echo "check-docs-claims: no milestone or status wording"

# 2. The plugin docs name the required checksum. A site page may be one file
#    or a directory of subpages.
for f in README.md skills/tobira/SKILL.md SECURITY.md site/src/pages/docs/write-a-driver site/src/pages/docs/cli; do
	if [ -f "$f.md" ]; then
		f="$f.md"
	fi
	if [ ! -e "$f" ]; then
		echo "check-docs-claims: $f(.md) not found" >&2
		exit 1
	fi
	if ! grep -rqiE 'sha256' "$f" || ! grep -rqiE 'plugin' "$f"; then
		echo "check-docs-claims: $f should document plugins and the required sha256" >&2
		exit 1
	fi
done
echo "check-docs-claims: plugin docs ok"

# 3. semantic-release strips changelogTitle from the top of CHANGELOG.md and
#    puts it back above each new release, so the two must match exactly.
python3 - <<'EOF'
import json, sys
plugins = json.load(open(".releaserc.json"))["plugins"]
title = next(p[1]["changelogTitle"] for p in plugins if isinstance(p, list) and p[0] == "@semantic-release/changelog")
if not open("CHANGELOG.md", encoding="utf-8").read().startswith(title + "\n"):
    sys.exit("check-docs-claims: CHANGELOG.md does not start with changelogTitle from .releaserc.json")
EOF
echo "check-docs-claims: changelog title ok"

# 4 to 8. Commands, flags, drivers and the README against the code.
export GOWORK=off
go test ./internal/cli -run 'TestDocsCoverCommandsAndFlags|TestUserDocsNameEveryCommand|TestHiddenCommandsAreDocumented|TestMCPSetupLinesUseRealFlags|TestReadme|TestContributingLayout' -count=1
(cd cmd/tobira && go test -run 'TestDriverTablesMatchTheBinary|TestDriverTablesAreIdentical|TestDocsDriverCountMatchesTheBinary' -count=1 .)
(cd ai && go test -run 'TestAIDocs|TestReadmeAIAndMCPClaims' -count=1 .)
echo "check-docs-claims: commands, flags, drivers, README and AI defaults ok"
