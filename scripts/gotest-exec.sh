#!/usr/bin/env bash
# Usage: go test -exec "$PWD/scripts/gotest-exec.sh" ...
# On macOS, signs the test binary ad hoc with the hypervisor entitlement and without hardened runtime, then runs it.
set -euo pipefail

if [[ "$(uname -s)" == Darwin ]]; then
	entitlements="$(dirname -- "${BASH_SOURCE[0]}")/../cmd/sbxr/entitlements.plist"

	if ! out="$(codesign --force --sign - --entitlements "$entitlements" "$1" 2>&1)"; then
		printf '%s\n' "$out" >&2
		exit 1
	fi
fi

exec "$@"
