#!/usr/bin/env bash
# Checks that an sbxr built on a hosted macOS runner carries the hypervisor entitlement, and that doctor sees it.
# Usage: scripts/ci/check-signature.sh <sbxr-binary>
set -euo pipefail

sbxr=$1
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

codesign -d --entitlements - --xml "$sbxr" >"$tmp/entitlements.plist"
if ! grep -q com.apple.security.hypervisor "$tmp/entitlements.plist"; then
	echo "$sbxr has no hypervisor entitlement" >&2
	exit 1
fi

# The runner has no hypervisor, so doctor must fail that check while the entitlement check passes.
if "$sbxr" doctor --json >"$tmp/doctor.json"; then
	echo "sbxr doctor passed, but this runner has no hypervisor" >&2
	exit 1
fi
jq -e '.checks[] | select(.id == "hypervisor") | .status == "fail"' "$tmp/doctor.json"
jq -e '.checks[] | select(.id == "entitlement") | .status == "pass"' "$tmp/doctor.json"
