# sbxr on Kubernetes

`sbxr` adds the RuntimeClass `sbxr`. A pod opts in with `runtimeClassName: sbxr` and runs in a microVM. The `sbxr` containerd runtime runs one VM per pod, with a guest agent and `crun` inside the guest.

## Requirements

- Kubernetes 1.36 or later, the supported minimum. CI tests Kubernetes 1.37 in kind.
- Nodes with containerd 2.2 or later, glibc 2.35 or later, Linux 5.19 or later, `/dev/kvm`, and `/dev/net/tun`.
- `iptables` and `nsenter` on the node for `kubectl port-forward`.
- A `kubelet` id range, so that pods with user namespaces (`hostUsers: false`) and `sbxr` pod VMs never share host ids. Without it, the kubelet may give such pods any host id. The kubelet reads the range with `getsubids kubelet` and `getsubids -g kubelet` at start, and so does preflight. The range must follow the kubelet's own rules for the default 65536 ids per pod:
  - The `kubelet` user exists, and `getsubids` is installed (Debian and Ubuntu package `uidmap`).
  - `getsubids` reports one range for uids and the same range for gids.
  - The first id is a multiple of 65536 and at least 65536.
  - The length is `maxPods × 65536`, or more in steps of 65536, where `maxPods` is the kubelet's pod limit (the node's `status.capacity.pods`, 110 by default).

  For example, on a node with the default 110 pods:

  ```sh
  useradd --system --no-create-home --shell /usr/sbin/nologin kubelet
  echo kubelet:65536:7208960 | tee -a /etc/subuid /etc/subgid   # 7208960 = 110 × 65536
  systemctl restart kubelet
  ```

  Restart the kubelet after you add or change the range. Preflight cannot tell whether the running kubelet has read it.

- One of these node distributions:
  - kubeadm-style nodes whose `/etc/containerd/config.toml` imports `/etc/containerd/conf.d/*.toml`.
  - k3s and RKE2, which import `config-v3.toml.d/*.toml` next to their containerd config.
  - k0s 1.36 or later, which imports `/etc/k0s/containerd.d/*.toml`.
  - kind, with the cluster config below.

## Install

```sh
kubectl apply -f https://github.com/albertsko/sbxr/releases/latest/download/sbxr-install.yaml
kubectl -n sbxr-system rollout status ds/sbxr-node
kubectl get nodes -L sbxr.dev/ready
```

The DaemonSet runs on every node. On each node, it:

1. Detects the distribution from its active systemd units.
2. Runs preflight: `/dev/kvm`, glibc, containerd version, main config version, an `imports` entry that covers the drop-in directory, the Linux version, `/dev/net/tun`, and the `kubelet` id range. It reserves the `sbxr` id range in `/etc/subuid` and `/etc/subgid`.
3. On failure, changes nothing and writes the reason to the node annotation `sbxr.dev/preflight`. It never edits a main containerd config.
4. Copies the bundle to a new directory below `/opt/sbxr.old`, checks every file against its SHA-256, and swaps it with `/opt/sbxr`, so the previous bundle stays in `/opt/sbxr.old` (see "Upgrade"). It runs `/opt/sbxr/bin/containerd-shim-sbxr-v1 check` on the host, which loads `libkrun` and `libkrunfw` from the bundle, and writes the reason to `sbxr.dev/preflight` when that fails. It links the shim to `/usr/local/bin/containerd-shim-sbxr-v1`, because containerd finds a sandboxer shim only by its name. Then it writes the drop-in `sbxr.toml` atomically, under a host lock, and restarts the containerd unit only when the drop-in changed.
5. Waits until the unit is active and the restarted containerd serves runtime handler `sbxr`, which it asks through the CRI `Status` call on the containerd socket. Then it sets the label `sbxr.dev/ready=true`. If the restart or this check fails, it restores the previous drop-in (or removes it) and restarts containerd again.

Read the result of a node:

```sh
kubectl get node NODE -o jsonpath='{.metadata.annotations.sbxr\.dev/preflight}'
```

To retry after you fix a node, run `kubectl -n sbxr-system rollout restart ds/sbxr-node`.

An install takes seconds per node, an upgrade too: the installer asks containerd itself, so it does not wait for the kubelet to post a node status. It gives the restarted containerd up to 5 minutes to serve the handler, which only matters when containerd starts slowly or fails. An unchanged bundle and drop-in cause no copy and no restart.

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

- The pod's memory limit (the sum of its container limits) sets the VM memory. Without a limit, the VM gets 1024 MiB. The pod's CPU limit, rounded up, sets the vCPUs.
- The RuntimeClass schedules pods only on nodes with `sbxr.dev/ready=true` and adds a fixed memory overhead for the VM to each pod.
- Admission policies reject `sbxr` pods that ask for privileged mode, the host network, host PID, host IPC, or added capabilities.
- The pod IP belongs to the guest. TCP, UDP, and ICMP work, and so do `kubectl exec`, exec probes, `kubectl port-forward`, and SIGTERM with the full grace period.
- A command that does not exist or cannot run is a start error (reason `StartError`), as with runc.
- The containers of a pod share `localhost`, the hostname, and IPC. Each has its own PID namespace.

## Limits

- No per-container metrics. `kubectl top` shows the pod's cgroup, which holds the VM.
- `shareProcessNamespace` is ignored.
- No `kubectl attach`.
- In-place pod resize is accepted, but the pod VM keeps its vCPUs and memory.

## Upgrade

Apply the `sbxr-install.yaml` of the new release and wait for the rollout as in "Install". Running pods need no drain: they keep working, with `kubectl exec`, exec probes, and graceful stop.

The installer keeps the previous bundle in `/opt/sbxr.old` instead of removing it. A running VM keeps its guest root and its agent's `crun` there, and its shim keeps its binaries and libraries. Each install, and uninstall, removes the retired bundles that no process uses any more, so a retired bundle goes at the first install after the kubelet removed the sandboxes of its last pods, which it does about a minute after their deletion. New pods use the new bundle.

## Uninstall

Delete `sbxr` pods first. Then:

```sh
kubectl apply -f https://github.com/albertsko/sbxr/releases/latest/download/sbxr-uninstall.yaml
kubectl -n sbxr-system rollout status ds/sbxr-node
kubectl delete -f https://github.com/albertsko/sbxr/releases/latest/download/sbxr-uninstall.yaml
```

The first command replaces the installer with a DaemonSet that removes the label and annotation, removes the drop-in of every supported distribution, restarts containerd, and removes `/opt/sbxr` and `/opt/sbxr.old`. It also removes the shim link and the `sbxr` id range. While `sbxr` pods still exist under `/run/sbxr/pods`, a process still uses a retired bundle, or containerd cannot restart, it keeps `/opt/sbxr`, writes the reason to `sbxr.dev/preflight`, and retries. It keeps `/run/sbxr.lock`, which goes away at reboot. It removes `/var/lib/sbxr/verify-after`, which older installers left. The last command removes the RuntimeClass, the policies, and the namespace.

## kind

kind needs two changes at cluster creation, both in [kind.yaml](../kind.yaml): the `/dev/kvm` mount and an `imports` entry for `/etc/containerd/conf.d/*.toml`. kind writes a version 2 containerd config, so the installer writes a version 2 drop-in.

kind resets the `/dev/kvm` permissions, so open them again after you create the cluster:

```sh
kind create cluster --config kind.yaml
sudo chmod 666 /dev/kvm
```

`mise run test:kind` creates the cluster, builds the installer image, and runs the tests.

## Contract

Tools, policies, and pod specs can depend on these names and values.

### Cluster objects

- Release assets: `sbxr-install.yaml` and `sbxr-uninstall.yaml`. Both pin the installer image `ghcr.io/albertsko/sbxr-node` by digest.
- Namespace `sbxr-system`, with the DaemonSet and ServiceAccount `sbxr-node`.
- RuntimeClass `sbxr`:
  - `handler: sbxr`.
  - `scheduling.nodeSelector`: `sbxr.dev/ready: "true"`.
  - `overhead.podFixed.memory`: `64Mi`, for the guest kernel and the VMM.
  - Annotation `sbxr.dev/limits`: a comma-separated list of the limits in "Limits".
  - Annotation `sbxr.dev/docs`: the URL of this guide.
- Node label `sbxr.dev/ready=true`: the node passed preflight, and its containerd serves handler `sbxr`. The installer removes the label when a node fails.
- Node annotation `sbxr.dev/preflight`: `ok` after a successful install, otherwise the reason why the node is not ready or why uninstall waits.

### Admission policies

- `sbxr-deny` (ValidatingAdmissionPolicy, action `Deny`) applies to pods with `runtimeClassName: sbxr` on create and update, including ephemeral containers that `kubectl debug` adds. It rejects:
  - the host network, the host PID namespace, and the host IPC namespace.
  - privileged mode in any container, init container, or ephemeral container.
  - added capabilities in any container, init container, or ephemeral container.
- `sbxr-node-self` (ValidatingAdmissionPolicy, action `Deny`) lets the installer's ServiceAccount change only the node that its pod runs on.

### containerd

- The installer writes the drop-in `sbxr.toml` into the drop-in directory of the distribution. It uses the version of the main config (2 or 3), because containerd rejects a version 3 drop-in under a version 2 main config.
- The drop-in adds runtime `sbxr` with `runtime_type = "io.containerd.sbxr.v1"`, `runtime_path = "/opt/sbxr/bin/containerd-shim-sbxr-v1"`, and `sandboxer = "shim"`. The shim is the pod sandboxer, so an `sbxr` pod has no pause container.
- containerd pulls the container images, not `sbxr`.

### Host paths

- `/opt/sbxr`: the bundle. `/opt/sbxr.old`: retired bundles, and new bundles while the installer stages them.
- `/usr/local/bin/containerd-shim-sbxr-v1`: a link to the shim in the bundle.
- `/run/sbxr/pods/<id>`: one directory per `sbxr` pod while it exists.
- `/run/sbxr.lock`: the installer lock.
- `/etc/subuid` and `/etc/subgid`: the entry `sbxr:<first>:33554432`, room for 512 pod VMs of 65536 ids each. The installer places it above the host ids (0 to 65535), aligned to 65536, and clear of every other entry and of the `kubelet` range. Uninstall removes it.
