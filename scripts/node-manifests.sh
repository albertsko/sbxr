#!/usr/bin/env bash
# Renders the template deploy/node/sbxr.yaml into the release assets sbxr-install.yaml and sbxr-uninstall.yaml.
# Usage: scripts/node-manifests.sh <image> <out-dir> [phase]
# Releases pass the installer image pinned by digest, for example ghcr.io/albertsko/sbxr-node:v1.2.3@sha256:....
# The phase defaults to 2. Lines that end in "# phase N" and blocks between "# phase N begin" and "# phase N end" belong to phase N only.
set -euo pipefail

template="$(dirname -- "${BASH_SOURCE[0]}")/../deploy/node/sbxr.yaml"
image=$1
out=$2
phase=${3:-2}
other=$((3 - phase))

mkdir -p "$out"
for mode in install uninstall; do
	sed -e "/^# phase $other begin\$/,/^# phase $other end\$/d" -e "/ # phase $other\$/d" -e "/^# phase $phase \(begin\|end\)\$/d" \
		-e "s/ # phase $phase\$//" -e "s|@IMAGE@|$image|" -e "s|@MODE@|$mode|" -e "s|@PHASE@|$phase|" "$template" >"$out/sbxr-$mode.yaml"
done
