#!/usr/bin/env bash
# Checks an sbxr installed with Homebrew on a hosted macOS runner, which has no hypervisor.
# Usage: scripts/ci/check-brew-install.sh
set -euo pipefail

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# brew install with the fully qualified name trusts only the formula, not the whole tap.
brew trust --json=v1 | tee "$tmp/trust.json"
jq -e '(.formulae | index("albertsko/tap/sbxr")) and (.taps | index("albertsko/tap") | not)' "$tmp/trust.json"

# The runner has no hypervisor, so doctor must fail, and on that check only. The first run prints the report for the log.
sbxr doctor || true
if sbxr doctor --json >"$tmp/doctor.json"; then
	echo "sbxr doctor passed, but this runner has no hypervisor" >&2
	exit 1
fi
jq -e '[.checks[] | select(.status == "fail") | .id] == ["hypervisor"]' "$tmp/doctor.json"
