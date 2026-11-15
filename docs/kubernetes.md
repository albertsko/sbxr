# sbxr on Kubernetes

`sbxr` adds the RuntimeClass `sbxr`. A pod opts in with `runtimeClassName: sbxr`, and each of its app containers runs in its own microVM. This page covers phase 1, which uses `crun` with `libkrun`.

## Requirements

- Kubernetes 1.36 or later. The DNS policy uses `MutatingAdmissionPolicy` at `admissionregistration.k8s.io/v1`, which older versions do not serve by default.
- Nodes with containerd 2.2 or later, glibc 2.35 or later, and `/dev/kvm`.
- One of these node distributions:
  - kubeadm-style nodes whose `/etc/containerd/config.toml` imports `/etc/containerd/conf.d/*.toml`.
  - k3s and RKE2, which import `config-v3.toml.d/*.toml` next to their containerd config.
  - k0s 1.36 or later, which imports `/etc/k0s/containerd.d/*.toml`.
  - kind, with the cluster config below.
- When runc uses systemd cgroups (`SystemdCgroup = true`), a D-Bus system socket at `/run/dbus/system_bus_socket`. `crun` reaches systemd only through D-Bus.

## Install

```sh
kubectl apply -f https://github.com/albertsko/sbxr/releases/latest/download/sbxr-install.yaml
kubectl -n sbxr-system rollout status ds/sbxr-node
kubectl get nodes -L sbxr.dev/ready
```

The DaemonSet runs on every node. On each node, it:

1. Detects the distribution from its active systemd units.
2. Runs preflight: `/dev/kvm`, glibc, containerd version, main config version, an `imports` entry that covers the drop-in directory, and D-Bus when runc uses systemd cgroups.
3. On failure, changes nothing and writes the reason to the node annotation `sbxr.dev/preflight`. It never edits a main containerd config.
4. Copies the bundle to `/opt/sbxr.new`, checks every file against its SHA-256, and swaps it in. Then it writes the drop-in `sbxr.toml` atomically, under a host lock, and restarts the containerd unit only when the drop-in changed.
5. Waits until the unit is active and the kubelet posts a node status, after the restart, that lists runtime handler `sbxr`. Then it sets the label `sbxr.dev/ready=true`. If the restart or this check fails, it restores the previous drop-in (or removes it) and restarts containerd again.

Read the result of a node:

```sh
kubectl get node NODE -o jsonpath='{.metadata.annotations.sbxr\.dev/preflight}'
```

To retry after you fix a node, run `kubectl -n sbxr-system rollout restart ds/sbxr-node`.

A first install takes seconds per node, because only the restarted containerd can list handler `sbxr`. When an `sbxr` drop-in already existed, the installer waits for a node status that the kubelet posts at least 10 seconds after the restart, which can take up to 5 minutes, so an upgrade rolls through the nodes at up to about 6 minutes each. An unchanged bundle and drop-in cause no copy and no restart.

## Use

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: hello
spec:
  runtimeClassName: sbxr
  terminationGracePeriodSeconds: 5
  restartPolicy: Never
  containers:
    - name: hello
      image: alpine:3.22
      command: [echo, hi]
      resources:
        limits:
          memory: 512Mi
```

- The container memory limit sets the VM memory. Without a limit, the VM gets 1024 MiB.
- The RuntimeClass schedules pods only on nodes with `sbxr.dev/ready=true` and adds a fixed memory overhead for the VM to each pod.
- Admission policies reject `sbxr` pods that ask for privileged mode, the host network, host PID, host IPC, or added capabilities. They warn about exec probes, exec lifecycle hooks, and grace periods over 10 seconds.
- A mutating policy adds the DNS option `use-vc` to `sbxr` pods.

## Phase 1 limits

- No `kubectl exec` and no exec probes.
- SIGTERM does not reach the workload, so pod deletion waits the full grace period. Use `terminationGracePeriodSeconds` of 5 or less.
- `kubectl logs` shows stdout and stderr together, and the order across the two streams is not kept.
- DNS works over TCP only. Programs that use connected UDP sockets fail without `use-vc`, and busybox `nslookup` fails.
- A command that does not exist or cannot run reports exit code 127 or 126 with reason `Error`. The log line `Couldn't execute '<path>' inside the vm: <reason>` names the cause.
- The workload is not PID 1, so a signal that it sends to itself can end it.
- One VM per container.
- ICMP, IPv6, and UDP listeners do not work.

## Uninstall

Delete `sbxr` pods first. Then:

```sh
kubectl apply -f https://github.com/albertsko/sbxr/releases/latest/download/sbxr-uninstall.yaml
kubectl -n sbxr-system rollout status ds/sbxr-node
kubectl delete -f https://github.com/albertsko/sbxr/releases/latest/download/sbxr-uninstall.yaml
```

The first command replaces the installer with a DaemonSet that removes the label and annotation, removes the drop-in of every supported distribution, restarts containerd, and removes `/opt/sbxr`. While `sbxr` containers still exist (state under `/run/sbxr/runc`, which the drop-in sets as the runtime root), or when containerd cannot restart, it keeps `/opt/sbxr`, writes the reason to `sbxr.dev/preflight`, and retries. It keeps `/run/sbxr.lock`, which goes away at reboot, and `/var/lib/sbxr/verify-after`, a timestamp that makes a later reinstall wait for a node status newer than this restart. If the node clock was ahead when that timestamp was written, delete the file to reinstall at once. The last command removes the RuntimeClass, the policies, and the namespace.

## kind

kind needs two changes at cluster creation, both in [kind.yaml](../kind.yaml): the `/dev/kvm` mount and an `imports` entry for `/etc/containerd/conf.d/*.toml`. kind writes a version 2 containerd config, so the installer writes a version 2 drop-in.

The kind node image has no D-Bus. Install it in each node before you install `sbxr`:

```sh
kind create cluster --config kind.yaml
sudo chmod 666 /dev/kvm   # kind resets the permissions
for node in $(kind get nodes); do
  docker exec "$node" sh -c 'apt-get update -qq && apt-get install -y -qq dbus && systemctl start dbus'
done
```

`mise run test:kind` does all of this, builds the installer image, and runs the tests.
