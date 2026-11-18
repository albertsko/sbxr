#!/usr/bin/env bash
# Writes the Go cache paths, key, and restore keys to GITHUB_OUTPUT, and points golangci-lint at its cached directory.
# Usage: scripts/ci/go-cache.sh (from the setup action, after mise installs Go)
# One key per OS, architecture, Go version, go.sum, and ISO week: main saves at most one entry a week, and GitHub evicts unused ones after 7 days.
set -euo pipefail

cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.."

gocache=$(go env GOCACHE)
lintcache=$(dirname -- "$gocache")/golangci-lint
prefix=go-$RUNNER_OS-$RUNNER_ARCH-$(go env GOVERSION)
modules=$(git hash-object go.sum | cut -c1-12)

echo "GOLANGCI_LINT_CACHE=$lintcache" >>"$GITHUB_ENV"
{
	echo "paths<<EOF"
	echo "$gocache"
	go env GOMODCACHE
	echo "$lintcache"
	echo "EOF"
	echo "key=$prefix-$modules-$(date -u +%G-W%V)"
	echo "restore-keys<<EOF"
	echo "$prefix-$modules-"
	echo "$prefix-"
	echo "EOF"
} >>"$GITHUB_OUTPUT"
