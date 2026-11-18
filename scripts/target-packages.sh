#!/usr/bin/env bash
# Usage: scripts/target-packages.sh TARGET
# Prints the packages that have a TARGET test file (*_TARGET_test.go) when built with the build tag TARGET.
# The target tasks test only these packages, because go test runs the TestMain of every package it gets.
set -euo pipefail

target="$1"

# One line per test file: the package, then the file name. The template is for go list, not the shell.
# shellcheck disable=SC2016
template='{{$p := .ImportPath}}{{range .TestGoFiles}}{{$p}} {{.}}
{{end}}{{range .XTestGoFiles}}{{$p}} {{.}}
{{end}}'

pkgs="$(go list -tags "$target" -f "$template" ./... | awk -v suffix="_${target}_test.go" 'substr($2, length($2) - length(suffix) + 1) == suffix { print $1 }' | sort -u)"

if [[ -z "$pkgs" ]]; then
	printf 'target-packages: no package has a *_%s_test.go file\n' "$target" >&2
	exit 1
fi

printf '%s\n' "$pkgs"
