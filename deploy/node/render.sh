#!/usr/bin/env bash
# Renders the template deploy/node/sbxr.yaml into the release assets sbxr-install.yaml and sbxr-uninstall.yaml.
# Usage: deploy/node/render.sh <image> <out-dir>
# Releases pass the installer image pinned by digest, for example ghcr.io/albertsko/sbxr-node:v1.2.3@sha256:....
set -euo pipefail

template="$(dirname -- "${BASH_SOURCE[0]}")/sbxr.yaml"
image=$1
out=$2

mkdir -p "$out"
for mode in install uninstall; do
	sed -e "s|@IMAGE@|$image|" -e "s|@MODE@|$mode|" "$template" >"$out/sbxr-$mode.yaml"
done
