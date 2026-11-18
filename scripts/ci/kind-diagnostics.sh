#!/usr/bin/env bash
# Prints the state of the kind cluster after a failed Kubernetes test and exports the kind logs to <out-dir>.
# Usage: scripts/ci/kind-diagnostics.sh <out-dir>
set -euo pipefail

out=$1
cluster=${SBXR_KIND_CLUSTER:-sbxr}
kubectl=(kubectl --context "kind-$cluster")

# A broken cluster is the usual reason to run this, so every command runs even when an earlier one fails.
try() {
	"$@" || echo "diagnostics: failed: $*" >&2
}

runtime_handlers() {
	"${kubectl[@]}" get node -o yaml | grep -E 'sbxr|runtimeHandlers' -A2
}

vm_logs() {
	docker exec "$cluster-control-plane" sh -c 'tail -n 30 /run/sbxr/pods/*/vm.log'
}

try runtime_handlers
try "${kubectl[@]}" get pods -A -o wide
try "${kubectl[@]}" -n sbxr-system logs ds/sbxr-node
try "${kubectl[@]}" -n sbxr-test describe pods
try vm_logs
try kind export logs --name "$cluster" "$out"
