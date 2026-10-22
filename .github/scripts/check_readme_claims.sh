#!/usr/bin/env bash
# Fails if the README (or the file given as $1) claims a capability that does
# not exist yet. The words below may appear only on a line that also says
# "planned", or inside a section whose heading says "planned".
# Matching is whole-word and case-insensitive.
set -euo pipefail

file="${1:-README.md}"
# The guarded words are secret providers that the binary does not compile in.
# Everything the README names outside them is checked against the code by the
# Go tests that check_docs_claims.sh runs (commands, flags, drivers, counts).
words='op://|aws-sm://|vault://'

if [ ! -f "$file" ]; then
	echo "check-readme-claims: $file not found" >&2
	exit 1
fi

# Drop every line that is allowed to mention a future capability, then look
# for the guarded words in what is left. A heading containing "planned" opens
# an allowed section that lasts until the next heading of the same or a
# higher level. Headings inside code fences are not headings.
offenders="$(
	awk '
		/^```/ { fence = !fence }
		!fence && /^#+ / {
			match($0, /^#+/)
			level = RLENGTH
			if (planned_level && level <= planned_level) planned_level = 0
			if (tolower($0) ~ /planned/ && !planned_level) planned_level = level
		}
		planned_level { next }
		tolower($0) ~ /planned/ { next }
		{ printf "%d:%s\n", NR, $0 }
	' "$file" | grep -wiE "$words" || true
)"

if [ -n "$offenders" ]; then
	echo "check-readme-claims: $file claims unshipped capabilities outside a 'planned' line or section:" >&2
	printf '%s\n' "$offenders" >&2
	exit 1
fi
echo "check-readme-claims: $file ok"
