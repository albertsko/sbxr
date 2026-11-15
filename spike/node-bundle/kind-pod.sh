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
# sbxr: systemd cgroups, which crun reaches only through D-Bus. sbxr-cgfs: cgroupfs.
docker exec "$node" sh -c 'cat >/etc/containerd/conf.d/sbxr.toml <<TOML
version = 2
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.sbxr]
  runtime_type = "io.containerd.runc.v2"
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.sbxr.options]
    BinaryName = "/opt/sbxr/bin/krun"
    SystemdCgroup = true
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.sbxr-cgfs]
  runtime_type = "io.containerd.runc.v2"
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.sbxr-cgfs.options]
    BinaryName = "/opt/sbxr/bin/krun"
TOML
systemctl restart containerd || { journalctl -u containerd --no-pager | tail -20; exit 1; }'

# The kind node image has no D-Bus. crun needs it for systemd cgroups, and runc does not.
docker exec "$node" sh -c 'apt-get update -qq && apt-get install -y -qq --no-install-recommends dbus >/dev/null && systemctl start dbus && ls -l /run/dbus/system_bus_socket'
docker exec "$node" sh -c 'grep -i cgroupDriver /var/lib/kubelet/config.yaml'
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

for h in sbxr sbxr-cgfs; do
  kubectl apply -f - <<YAML
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: {name: sbxr-cgfs}
handler: sbxr-cgfs
---
apiVersion: v1
kind: Pod
metadata: {name: sleep-$h}
spec:
  runtimeClassName: $h
  containers:
    - name: c
      image: "alpine:3.22"
      command: [sleep, "600"]
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
# Where each handler puts the VM process: the pod cgroup under kubepods.slice, or somewhere else.
for h in sbxr sbxr-cgfs; do
  kubectl wait --for=condition=Ready "pod/sleep-$h" --timeout=120s >/dev/null || kubectl describe pod "sleep-$h" | tail -5
  uid=$(kubectl get pod "sleep-$h" -o jsonpath='{.metadata.uid}' | tr - _)
  cid=$(kubectl get pod "sleep-$h" -o jsonpath='{.status.containerStatuses[0].containerID}' | sed 's|.*://||')
  docker exec "$node" sh -c "pid=\$(crictl inspect -o go-template --template '{{.info.pid}}' $cid); cg=\$(cut -d: -f3 /proc/\$pid/cgroup); echo \"RESULT handler $h: vm pid \$pid comm \$(cat /proc/\$pid/comm) cgroup \$cg memory.max \$(cat /sys/fs/cgroup\$cg/memory.max)\"; echo \"pod $uid under kubepods: \$(find /sys/fs/cgroup/kubepods.slice -type d -name \"*$uid*\" | head -1)\""
done

[[ $(kubectl logs hi | head -1) == hi && $code == 3 ]]
