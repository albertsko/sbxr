#!/usr/bin/env bash
# A2: runs the bundle in a kind cluster through containerd and a drop-in, then runs two pods.
# Usage: kind-pod.sh <bundle-dir> <node-image>
set -euo pipefail

bundle=$(realpath "$1")
image=$2
node=kind-control-plane

cat >/tmp/kind.yaml <<YAML
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
containerdConfigPatches:
  - |-
    imports = ["/etc/containerd/conf.d/*.toml"]
nodes:
  - role: control-plane
    extraMounts:
      - hostPath: /dev/kvm
        containerPath: /dev/kvm
      - hostPath: $bundle
        containerPath: /opt/sbxr
        readOnly: true
YAML
kind create cluster --image "$image" --config /tmp/kind.yaml --wait 120s
sudo chmod 666 /dev/kvm

echo "::group::rendered containerd config"
docker exec "$node" sh -c 'containerd --version; cat /etc/containerd/config.toml'
echo "::endgroup::"

docker exec "$node" sh -c 'mkdir -p /etc/containerd/conf.d && cat >/etc/containerd/conf.d/sbxr.toml <<TOML
version = 3
[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.sbxr]
  runtime_type = "io.containerd.runc.v2"
  [plugins."io.containerd.cri.v1.runtime".containerd.runtimes.sbxr.options]
    BinaryName = "/opt/sbxr/bin/krun"
    SystemdCgroup = true
TOML
systemctl restart containerd'
sleep 5
docker exec "$node" sh -c 'crictl info | grep -A6 "\"sbxr\""'
docker exec "$node" /opt/sbxr/bin/krun --version
kubectl wait --for=condition=Ready node --all --timeout=120s
kubectl get node -o jsonpath='{.items[0].status.runtimeHandlers}'
echo

kubectl apply -f - <<'YAML'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: {name: sbxr}
handler: sbxr
---
apiVersion: v1
kind: Pod
metadata: {name: hi}
spec:
  runtimeClassName: sbxr
  restartPolicy: Never
  containers:
    - {name: c, image: "alpine:3.22", command: [sh, -c, "echo hi; uname -r"]}
---
apiVersion: v1
kind: Pod
metadata: {name: exit3}
spec:
  runtimeClassName: sbxr
  restartPolicy: Never
  containers:
    - {name: c, image: "busybox:1.37", command: [sh, -c, "echo hi; exit 3"]}
YAML

kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/hi --timeout=180s || kubectl describe pod hi
kubectl wait --for=jsonpath='{.status.phase}'=Failed pod/exit3 --timeout=180s || kubectl describe pod exit3
echo "hi logs: $(kubectl logs hi | tr '\n' ' ')"
echo "exit3 logs: $(kubectl logs exit3)"
code=$(kubectl get pod exit3 -o jsonpath='{.status.containerStatuses[0].state.terminated.exitCode}')
echo "exit3 exitCode: $code"
echo "host kernel: $(uname -r)"

[[ $(kubectl logs hi | head -1) == hi && $code == 3 ]]
