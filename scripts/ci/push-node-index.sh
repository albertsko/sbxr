#!/usr/bin/env bash
# Joins the per-architecture node images into one index tagged <image>:<tag>, then renders the release manifests pinned to its digest.
# Usage: scripts/ci/push-node-index.sh <image> <tag> <digest-dir> <out-dir>
# <digest-dir> holds one file per architecture with the digest that the image build pushed.
set -euo pipefail

cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.."

image=$1
ref=$image:$2
digests=$3
out=$4

sources=()
for f in "$digests"/*; do
	sources+=("$image@$(cat "$f")")
done

docker buildx imagetools create -t "$ref" "${sources[@]}"
digest=$(docker buildx imagetools inspect "$ref" --format '{{json .Manifest}}' | jq -r .digest)
deploy/node/render.sh "$ref@$digest" "$out"
