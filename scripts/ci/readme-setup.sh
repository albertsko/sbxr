#!/usr/bin/env bash
# Runs the Linux setup block from README.md unchanged, except that it downloads the archive from the goreleaser snapshot in dist/.
# Usage: scripts/ci/readme-setup.sh (after goreleaser release --snapshot --clean)
set -euo pipefail

cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.."

release_url=https://github.com/albertsko/sbxr/releases/latest/download
snapshot_url=file://$PWD/dist
setup=$(mktemp)
trap 'rm -f "$setup"' EXIT

# The first sh block after the heading "### Linux".
awk '/^### Linux/ { s = 1 } s && /^```sh$/ { f = 1; next } f && /^```$/ { exit } f' README.md >"$setup"
if ! grep -qF "$release_url" "$setup"; then
	echo "The Linux setup block in README.md no longer downloads from $release_url" >&2
	exit 1
fi
sed -i "s|$release_url|$snapshot_url|" "$setup"
cat "$setup"

eval "$(/home/linuxbrew/.linuxbrew/bin/brew shellenv)"
bash -euxo pipefail "$setup"
