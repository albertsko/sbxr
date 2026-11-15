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

# Evidence: a version 3 drop-in under the version 2 main file that kind writes.
docker exec "$node" sh -c 'mkdir -p /etc/containerd/conf.d && cat >/etc/containerd/conf.d/sbxr.toml <<TOML
version = 3
[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.sbxr]
  runtime_type = "io.containerd.runc.v2"
TOML
systemctl restart containerd && echo "RESULT v3 drop-in under v2 main: containerd started" || { echo "RESULT v3 drop-in under v2 main: containerd failed"; journalctl -u containerd --no-pager | grep -i -E "error|fail|version" | tail -5; }'

# A drop-in uses the same config version as the main file.
# sbxr: wrapper with systemd cgroups. sbxr-link: crun link with systemd cgroups. sbxr-cgfs: crun link with cgroupfs.
docker exec "$node" sh -c 'ls -l /run/dbus/system_bus_socket /run/systemd/private; cat >/etc/containerd/conf.d/sbxr.toml <<TOML
version = 2
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.sbxr]
  runtime_type = "io.containerd.runc.v2"
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.sbxr.options]
    BinaryName = "/opt/sbxr/bin/krun"
    SystemdCgroup = true
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.sbxr-link]
  runtime_type = "io.containerd.runc.v2"
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.sbxr-link.options]
    BinaryName = "/opt/sbxr/libexec/krun"
    SystemdCgroup = true
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.sbxr-cgfs]
  runtime_type = "io.containerd.runc.v2"
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.sbxr-cgfs.options]
    BinaryName = "/opt/sbxr/libexec/krun"
TOML
systemctl restart containerd || { journalctl -u containerd --no-pager | tail -20; exit 1; }'
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

for h in sbxr-link sbxr-cgfs; do
  kubectl apply -f - <<YAML
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: {name: $h}
handler: $h
---
apiVersion: v1
kind: Pod
metadata: {name: hi-$h}
spec:
  runtimeClassName: $h
  restartPolicy: Never
  containers:
    - name: c
      image: "alpine:3.22"
      command: [sh, -c, "echo hi; cat /sys/fs/cgroup/memory.max 2>/dev/null; uname -r"]
      resources: {limits: {memory: 256Mi}}
YAML
done

kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/hi --timeout=180s || kubectl describe pod hi
kubectl wait --for=jsonpath='{.status.phase}'=Failed pod/exit3 --timeout=180s || kubectl describe pod exit3
echo "hi logs: $(kubectl logs hi | tr '\n' ' ')"
echo "exit3 logs: $(kubectl logs exit3)"
code=$(kubectl get pod exit3 -o jsonpath='{.status.containerStatuses[0].state.terminated.exitCode}')
echo "exit3 exitCode: $code"
echo "host kernel: $(uname -r)"
for h in sbxr-link sbxr-cgfs; do
  kubectl wait --for=jsonpath='{.status.phase}'=Succeeded "pod/hi-$h" --timeout=120s >/dev/null || true
  echo "RESULT handler $h: phase $(kubectl get pod "hi-$h" -o jsonpath='{.status.phase}') logs: $(kubectl logs "hi-$h" 2>/dev/null | tr '\n' ' ') event: $(kubectl get events --field-selector "involvedObject.name=hi-$h,type=Warning" -o jsonpath='{.items[0].message}' | cut -c1-220)"
done
docker exec "$node" sh -c 'for p in $(pgrep -f "^/opt/sbxr|krun"); do echo "proc $p $(cat /proc/$p/comm) $(cat /proc/$p/cgroup)"; done' || true

[[ $(kubectl logs hi | head -1) == hi && $code == 3 ]]
