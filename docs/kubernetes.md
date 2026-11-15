# sbxr on Kubernetes

`sbxr` adds the RuntimeClass `sbxr`. A pod opts in with `runtimeClassName: sbxr` and runs in a microVM. The release manifests install phase 2: the `sbxr` containerd runtime runs one VM per pod, with a guest agent and `crun` inside the guest. Phase 1 runs each app container in its own VM with `crun` and `libkrun` on the node, and has more limits (see "Phase 1"). Both phases use the same RuntimeClass, so pod specs stay the same.

## Requirements

- Kubernetes 1.36 or later, the supported minimum for both phases. Phase 1's DNS policy uses `MutatingAdmissionPolicy` at `admissionregistration.k8s.io/v1`, which older versions do not serve by default. CI tests Kubernetes 1.37 in kind.
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
4. Copies the bundle to a new directory below `/opt/sbxr.old`, checks every file against its SHA-256, and swaps it with `/opt/sbxr`, so the previous bundle stays in `/opt/sbxr.old` (see "Upgrade"), and links the shim to `/usr/local/bin/containerd-shim-sbxr-v1`, because containerd finds a sandboxer shim only by its name. Then it writes the drop-in `sbxr.toml` atomically, under a host lock, and restarts the containerd unit only when the drop-in changed.
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

## Upgrade

Apply the `sbxr-install.yaml` of the new release and wait for the rollout as in "Install". Running pods need no drain: they keep working, with `kubectl exec`, exec probes, and graceful stop.

The installer keeps the previous bundle in `/opt/sbxr.old` instead of removing it. A running phase 2 VM keeps its guest root and its agent's `crun` there, and older shims and phase 1 VMs keep their binaries and libraries. Each install, and uninstall, removes the retired bundles that no process uses any more, so a retired bundle goes at the first install after the kubelet removed the sandboxes of its last pods, which it does about a minute after their deletion. New pods use the new bundle.

### From phase 1

1. Configure the `kubelet` id range on every node and restart the kubelet, as in "Requirements". A node without a valid range fails phase 2 preflight: the installer writes the reason to `sbxr.dev/preflight` and removes `sbxr.dev/ready`, so no new `sbxr` pod is scheduled there. Its phase 1 drop-in and bundle stay, so `sbxr` pods that already run there keep running.
2. Apply the new `sbxr-install.yaml` and wait for the rollout as in "Install". Each node switches when its containerd restarts.
3. Recreate running `sbxr` pods, so that they move to phase 2.
4. Delete the two phase 1 policies, which `kubectl apply` keeps:

```sh
kubectl delete --ignore-not-found validatingadmissionpolicy,validatingadmissionpolicybinding sbxr-warn
kubectl delete --ignore-not-found mutatingadmissionpolicy,mutatingadmissionpolicybinding sbxr-dns-use-vc
```

## Phase 1

Render phase 1 manifests with `scripts/node-manifests.sh IMAGE OUT 1`. Phase 1 needs D-Bus on the node when runc uses systemd cgroups (`SystemdCgroup = true`), because `crun` reaches systemd only through D-Bus. It adds two admission policies: one warns about exec probes, exec lifecycle hooks, and grace periods over 10 seconds, and one adds the DNS option `use-vc`. Its limits:

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

The first command replaces the installer with a DaemonSet that removes the label and annotation, removes the drop-in of every supported distribution, restarts containerd, and removes `/opt/sbxr` and `/opt/sbxr.old`. It also removes the shim link and the `sbxr` id range. While `sbxr` pods still exist (a directory under `/run/sbxr/pods`, or phase 1 state under `/run/sbxr/runc`), or a process still uses a retired bundle, or when containerd cannot restart, it keeps `/opt/sbxr`, writes the reason to `sbxr.dev/preflight`, and retries. It keeps `/run/sbxr.lock`, which goes away at reboot. It removes `/var/lib/sbxr/verify-after`, which older installers left. The last command removes the RuntimeClass, the policies, and the namespace.

## kind

kind needs two changes at cluster creation, both in [kind.yaml](../kind.yaml): the `/dev/kvm` mount and an `imports` entry for `/etc/containerd/conf.d/*.toml`. kind writes a version 2 containerd config, so the installer writes a version 2 drop-in.

kind resets the `/dev/kvm` permissions, so open them again after you create the cluster:

```sh
kind create cluster --config kind.yaml
sudo chmod 666 /dev/kvm
```

For phase 1, the kind node image also needs D-Bus: run `apt-get install -y dbus && systemctl start dbus` in each node. `mise run test:kind` does all of this, builds the installer image, and runs the tests. `SBXR_PHASE=1` selects phase 1, and `SBXR_UPGRADE_FROM=1` installs phase 1 first and then upgrades.
