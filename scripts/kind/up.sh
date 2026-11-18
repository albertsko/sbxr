#!/usr/bin/env bash
# Creates or reuses the kind cluster for mise run test:kube, then installs sbxr with a locally built installer image.
# Usage: scripts/kind/up.sh
# Environment: SBXR_KIND_CLUSTER (default sbxr), SBXR_NODE_IMAGE (an installer image that is already built; default: build one).
set -euo pipefail

cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.."

cluster=${SBXR_KIND_CLUSTER:-sbxr}
node=$cluster-control-plane
kubectl=(kubectl --context "kind-$cluster")
tmp=$(mktemp -d)
# kind cannot load images with attestation manifests.
export BUILDX_NO_DEFAULT_ATTESTATIONS=1
trap 'rm -rf "$tmp"' EXIT

if ! kind get clusters | grep -qx "$cluster"; then
	kind create cluster --name "$cluster" --config scripts/kind/cluster.yaml --wait 2m
	# kind resets /dev/kvm permissions when it creates a cluster. Other jobs on this host need them back.
	if [[ ! -w /dev/kvm ]]; then
		sudo -n chmod 666 /dev/kvm || echo "warning: run sudo chmod 666 /dev/kvm" >&2
	fi
fi

# Preflight reads the kubelet id range with getsubids, from the uidmap package.
docker exec "$node" sh -c 'command -v getsubids >/dev/null ||
	{ export DEBIAN_FRONTEND=noninteractive && apt-get update -qq && apt-get install -y -qq --no-install-recommends uidmap >/dev/null; }'
# sbxr needs a kubelet id range for pods with user namespaces, apart from the sbxr range. The kubelet reads it at start.
docker exec "$node" sh -c 'getent passwd kubelet >/dev/null && grep -q "^kubelet:" /etc/subuid && exit 0
	getent passwd kubelet >/dev/null || useradd --system --no-create-home --shell /usr/sbin/nologin kubelet
	for f in /etc/subuid /etc/subgid; do grep -q "^kubelet:" "$f" 2>/dev/null || echo kubelet:65536:7208960 >>"$f"; done
	systemctl restart kubelet'
"${kubectl[@]}" wait node --all --for=condition=Ready --timeout=2m

image=${SBXR_NODE_IMAGE:-}
if [[ -z $image ]]; then
	image=sbxr-node:dev
	docker buildx build --load -q -f deploy/node/Dockerfile -t "$image" .
fi
# A tag per image ID makes the DaemonSet roll out when the image changes.
tag=sbxr-node:$(docker image inspect -f '{{.Id}}' "$image" | cut -c8-19)
docker tag "$image" "$tag"

# Images for the tests, built here, because kind fails to load multi-platform images. Pods use them with imagePullPolicy IfNotPresent.
for i in busybox:1.37 alpine:3.22 debian:bookworm-slim; do
	echo "FROM $i" | docker buildx build --load -q -t "sbxr-test/${i%%:*}:1" - >/dev/null
done
arch=$(docker version -f '{{.Server.Arch}}')
CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -o "$tmp/dnsgo" ./internal/scenario/testdata/dnsgo
printf 'FROM scratch\nCOPY dnsgo /dnsgo\nENTRYPOINT ["/dnsgo"]\n' >"$tmp/Dockerfile.dnsgo"
docker buildx build --load -q -f "$tmp/Dockerfile.dnsgo" -t sbxr-test/dnsgo:1 "$tmp" >/dev/null
kind load docker-image --name "$cluster" "$tag" sbxr-test/{busybox,alpine,debian,dnsgo}:1

# Install or upgrade, and wait until the node is ready.
deploy/node/render.sh "$tag" "$tmp"
"${kubectl[@]}" apply -f "$tmp/sbxr-install.yaml"
"${kubectl[@]}" -n sbxr-system rollout status ds/sbxr-node --timeout=8m
if ! "${kubectl[@]}" wait node --all --for=jsonpath='{.metadata.labels.sbxr\.dev/ready}'=true --timeout=10s; then
	"${kubectl[@]}" get node -o jsonpath='{.items[*].metadata.annotations.sbxr\.dev/preflight}{"\n"}' >&2
	"${kubectl[@]}" -n sbxr-system logs ds/sbxr-node >&2
	exit 1
fi
"${kubectl[@]}" create namespace sbxr-test --dry-run=client -o yaml | "${kubectl[@]}" apply -f -
