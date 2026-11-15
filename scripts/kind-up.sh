#!/usr/bin/env bash
# Creates or reuses the kind cluster for mise run test:kind, then installs sbxr with a locally built installer image.
# Usage: scripts/kind-up.sh
# Environment: SBXR_KIND_CLUSTER (default sbxr), SBXR_NODE_IMAGE (an installer image that is already built; default: build one),
# SBXR_PHASE (1 or 2, default 2), SBXR_UPGRADE_FROM (a phase to install first and then upgrade from, default none).
set -euo pipefail

cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."

cluster=${SBXR_KIND_CLUSTER:-sbxr}
phase=${SBXR_PHASE:-2}
node=$cluster-control-plane
kubectl=(kubectl --context "kind-$cluster")
tmp=$(mktemp -d)
# kind cannot load images with attestation manifests.
export BUILDX_NO_DEFAULT_ATTESTATIONS=1
trap 'rm -rf "$tmp"' EXIT

if ! kind get clusters | grep -qx "$cluster"; then
	kind create cluster --name "$cluster" --config kind.yaml --wait 2m
	# kind resets /dev/kvm permissions when it creates a cluster. Other jobs on this host need them back.
	if [[ ! -w /dev/kvm ]]; then
		sudo -n chmod 666 /dev/kvm || echo "warning: run sudo chmod 666 /dev/kvm" >&2
	fi
fi

# The kind node image has no D-Bus, and crun reaches systemd only through D-Bus.
docker exec "$node" sh -c 'test -S /run/dbus/system_bus_socket ||
	{ export DEBIAN_FRONTEND=noninteractive && apt-get update -qq && apt-get install -y -qq --no-install-recommends dbus >/dev/null && systemctl start dbus; }'

image=${SBXR_NODE_IMAGE:-}
if [[ -z $image ]]; then
	image=sbxr-node:dev
	docker build -q -f deploy/node/Dockerfile -t "$image" .
fi
# A tag per image ID makes the DaemonSet roll out when the image changes.
tag=sbxr-node:$(docker image inspect -f '{{.Id}}' "$image" | cut -c8-19)
docker tag "$image" "$tag"

# Images for the phase 1 tests, built here, because kind fails to load multi-platform images. Pods use them with imagePullPolicy IfNotPresent.
for i in busybox:1.37 alpine:3.22 debian:bookworm-slim; do
	echo "FROM $i" | docker build -q -t "sbxr-test/${i%%:*}:1" - >/dev/null
done
arch=$(docker version -f '{{.Server.Arch}}')
CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -o "$tmp/dnsgo" ./internal/scenario/testdata/dnsgo
printf 'FROM scratch\nCOPY dnsgo /dnsgo\nENTRYPOINT ["/dnsgo"]\n' >"$tmp/Dockerfile.dnsgo"
docker build -q -f "$tmp/Dockerfile.dnsgo" -t sbxr-test/dnsgo:1 "$tmp" >/dev/null
kind load docker-image --name "$cluster" "$tag" sbxr-test/{busybox,alpine,debian,dnsgo}:1

# install installs or upgrades to phase $1 and waits until the node is ready for it.
install() {
	scripts/node-manifests.sh "$tag" "$tmp/phase$1" "$1"
	"${kubectl[@]}" apply -f "$tmp/phase$1/sbxr-install.yaml"
	if [[ $1 == 2 ]]; then
		# Phase 2 needs neither phase 1 policy.
		"${kubectl[@]}" delete --ignore-not-found validatingadmissionpolicy,validatingadmissionpolicybinding sbxr-warn
		"${kubectl[@]}" delete --ignore-not-found mutatingadmissionpolicy,mutatingadmissionpolicybinding sbxr-dns-use-vc
	fi
	"${kubectl[@]}" -n sbxr-system rollout status ds/sbxr-node --timeout=8m
	if ! "${kubectl[@]}" wait node --all --for=jsonpath='{.metadata.labels.sbxr\.dev/ready}'=true --timeout=10s; then
		"${kubectl[@]}" get node -o jsonpath='{.items[*].metadata.annotations.sbxr\.dev/preflight}{"\n"}' >&2
		"${kubectl[@]}" -n sbxr-system logs ds/sbxr-node >&2
		exit 1
	fi
}

if [[ -n ${SBXR_UPGRADE_FROM:-} ]]; then
	install "$SBXR_UPGRADE_FROM"
fi
install "$phase"
"${kubectl[@]}" create namespace sbxr-test --dry-run=client -o yaml | "${kubectl[@]}" apply -f -
