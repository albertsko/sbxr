# sbxr: Product Requirements

Status: draft, written on the go. Last update: 2026-11-13.

## Problem Statement

Developers and platform teams want to run untrusted or experimental code, such as AI agent tools, CI jobs, or third-party images, with stronger isolation than a container gives. A container shares the host kernel. A virtual machine does not, but today a VM costs too much effort:

- Machine users must learn a large tool, install several helpers, and write configuration before they can run one command in isolation.
- Cluster users must install heavy runtimes (Kata, KubeVirt) or experimental projects that do not document Kubernetes use.
- Tools for the laptop and tools for the cluster behave differently, so a workload tested on a laptop can fail in the cluster.
- When a setup is broken (no hypervisor, missing library, missing macOS entitlement, missing user namespace support), tools often fail late with unclear errors.

## Solution

`sbxr` runs an OCI container image as a small virtual machine with its own Linux kernel, using `libkrun`. It has two front doors:

- **Machine:** a CLI. `sbxr run alpine echo hi` behaves like `docker run`, but the workload runs in a microVM. It works on macOS arm64 (Hypervisor.framework) and Linux (KVM), without root.
- **Kubernetes:** a RuntimeClass named `sbxr`. A pod opts in with `runtimeClassName: sbxr`. One manifest installs it on the nodes.

`sbxr` stays minimal. It runs a workload, streams its output, returns its exit code, and cleans up. It checks the environment before it starts any VM and stops with a clear fix hint when something is wrong.

Delivery has two phases:

- **Phase 1:** the CLI, plus a Kubernetes RuntimeClass backed by `crun` in its `libkrun` mode. Phase 1 Kubernetes runs one VM per container and has known gaps (no `kubectl exec`, no graceful stop).
- **Phase 2:** the `sbxr` containerd runtime. It runs one VM per pod with a small `sbxr` guest agent, and closes the phase 1 gaps. The RuntimeClass name `sbxr` stays the same, so pod specs do not change.

## User Stories

### Machine user (CLI)

1. As a developer, I want to install `sbxr` with one package manager command, so that I can start without building anything.
2. As a developer, I want `sbxr doctor` to check my machine and tell me the exact command to fix each problem, so that I do not debug setup issues myself.
3. As a developer, I want `sbxr run IMAGE CMD...` to work like `docker run`, so that I can reuse what I already know.
4. As a developer, I want the workload's stdout on stdout and `sbxr` messages on stderr, so that I can pipe workload output into other tools.
5. As a developer, I want `sbxr run` to exit with the workload's exit code, so that scripts and CI react to failures correctly.
6. As a developer, I want `sbxr run -it IMAGE sh` to give me an interactive shell, so that I can explore an image inside a VM.
7. As a developer, I want Ctrl-C to stop the VM and leave nothing running, so that I can trust that interrupted runs do not leak resources.
8. As a developer, I want the workload to have no network unless I ask for it, so that untrusted code cannot reach my host or my LAN by default.
9. As a developer, I want `--net` and `-p HOST:GUEST`, so that I can give a workload network access and publish a port when I need to.
10. As a developer, I want `-v HOST:GUEST[:ro]`, so that I can share a project directory with the workload.
11. As a developer, I want `-e KEY=VALUE` and `--env-file`, so that I can pass configuration without baking it into the image.
12. As a developer, I want `--cpus` and `--memory`, so that I can size the VM for the workload.
13. As a developer, I want invalid flags and values rejected before any VM starts, so that I get fast, clear feedback.
14. As a developer, I want a clear error when the image does not exist or does not match my CPU architecture, so that I do not wait for a VM that cannot work.
15. As a developer, I want images cached between runs and `sbxr prune` to clear the cache, so that repeated runs are fast and disk use stays under control.
16. As a developer, I want colors and progress only on a terminal, and plain output when piped or when `NO_COLOR` is set, so that logs stay readable.
17. As a developer, I want `--debug` to show detailed logs, so that I can report problems with useful context.
18. As a script author, I want `sbxr doctor --json`, so that automation can check readiness.
19. As a developer, I want to run several `sbxr run` commands at the same time, so that parallel jobs do not block or corrupt each other.
20. As a Linux developer, I want `sbxr` to run without root and still show correct file ownership in the guest, so that images that depend on root-owned files work.
21. As a developer, I want a cached `sbxr run alpine true` to finish in about a second, so that I can use VMs in tight loops.

### Cluster user (Kubernetes)

22. As a cluster admin, I want to install `sbxr` on nodes with one `kubectl apply`, so that I do not have to edit containerd config by hand.
23. As a cluster admin, I want only nodes that pass the `sbxr` checks to receive `sbxr` pods, so that pods never land on a node without a hypervisor.
24. As a cluster admin, I want one manifest to uninstall `sbxr`, so that I can remove it cleanly.
25. As a cluster admin, I want to move from phase 1 to phase 2 without changing pod specs, so that the upgrade is a node-level change only.
26. As an application developer, I want to opt in with `runtimeClassName: sbxr` only, so that I do not change my images or the rest of my pod spec.
27. As an application developer, I want `kubectl logs` to show workload output and the pod status to show the exit code, so that I debug the same way as with normal pods.
28. As an application developer, I want the containers of one pod to share one VM and `localhost`, so that sidecars work as in normal pods (phase 2).
29. As an application developer, I want `kubectl exec` and exec probes to work, so that I can debug and health-check workloads (phase 2).
30. As an application developer, I want pod deletion to send SIGTERM to my workload and wait for the grace period, so that rollouts shut down cleanly (phase 2).
31. As a platform team, I want `kubernetes-sigs/agent-sandbox` resources to work with `sbxr` as their runtime class, so that we can manage sandboxes with a standard API.

### Maintainer and contributor

32. As a maintainer, I want one command to run the same tests on macOS arm64 and on Ubuntu, so that local results match CI.
33. As a maintainer, I want behavior scenarios that run against a fake VM, a real CLI, and a kind cluster, so that the CLI and Kubernetes keep the same behavior.
34. As a maintainer, I want CI to fail when a hypervisor is expected but missing, so that VM tests are never skipped silently.
35. As an AI agent working on the code, I want dependency sources in a local `vendor/` directory, so that I can read them without network access.
36. As a maintainer, I want the core to have no CLI or Kubernetes code in it, so that we can add a web or TUI front door later.

## Acceptance Criteria

### Install and preflight

- After `brew install` on macOS arm64, `sbxr doctor` exits 0 on a machine with Hypervisor.framework support.
- `sbxr doctor` runs every check listed in the `doctor` contract and reports each one.
- When any check fails, `sbxr doctor` exits 1 and prints one line per failed check with a fix command.
- `sbxr run` runs the same checks first. When a check fails, `sbxr run` exits 125 and no VM process starts.
- On Ubuntu with unprivileged user namespaces restricted by AppArmor, `sbxr doctor` fails the `userns` check and names the fix.

### Run a workload (CLI)

- `sbxr run alpine echo hi` prints `hi` on stdout, prints nothing else on stdout, and exits 0.
- `sbxr run alpine sh -c 'exit 3'` exits 3.
- A command that does not exist in the image exits 127. A file that cannot be executed exits 126.
- The image `Entrypoint`, `Cmd`, `Env`, `WorkingDir`, and `User` apply. Arguments after the image replace `Cmd`. `-e` overrides image `Env`.
- Arguments with quotes, tabs, newlines, and non-ASCII characters reach the workload unchanged.
- The host environment never leaks into the guest. Only image env, `-e` and `--env-file` values, and `TERM` (with `-t`) are set.
- `--memory 0`, `--cpus 0`, more CPUs than the host has, an invalid image reference, or a malformed `-p` or `-v` value fail with exit 125 before any download or VM start.
- A missing image fails with exit 125, names the image, and leaves no partial cache entry.
- An image without a variant for the host architecture fails with exit 125 and names the available platforms.
- On macOS, an image layer with paths that differ only by letter case fails with exit 125.
- In the guest, files keep the owner, group, mode, setuid bits, and hardlinks from the image, on macOS and on Linux without root.
- After every run, including failed and interrupted runs, no VM process and no run state remain.
- With the image cached, `sbxr run alpine true` finishes in under 1 second on an Apple silicon Mac.

### Signals and interactive use

- Ctrl-C during `sbxr run` stops the VM and exits 130.
- SIGTERM to `sbxr run` stops the VM and exits 143.
- `sbxr run -it alpine sh` gives an interactive shell. Terminal resize reaches the guest.

### Networking and mounts

- Without `--net`, the workload cannot open any outbound connection.
- With `--net`, the workload reaches an IPv4 TCP server on the host network and resolves DNS names.
- `-p 8080:80` without `--net` fails with exit 125 before any VM start.
- `-p 8080:80` with `--net` makes the guest service reachable on host port 8080, and no other guest port is reachable.
- `-v ./dir:/work` makes host files visible and writable at `/work`. `-v ./dir:/work:ro` rejects writes.

### Output

- When stdout is not a terminal, or `NO_COLOR` is set, or `--no-color` is passed, output contains no ANSI escape codes.
- `sbxr` errors are one line on stderr that starts with `sbxr:`.
- A workload exit code, including a non-zero one, never prints any `sbxr` error text and never waits on terminal queries.

### Kubernetes, phase 1

- After `kubectl apply -f sbxr-install.yaml`, every node that passes preflight gets the label `sbxr.dev/ready=true`, and the RuntimeClass `sbxr` exists.
- A pod with `runtimeClassName: sbxr` and `restartPolicy: Never` that runs `echo hi` reaches phase `Succeeded`, and `kubectl logs` shows `hi`.
- A pod whose container exits 3 reports `exitCode: 3` in its container status.
- A pod with `runtimeClassName: sbxr` is never scheduled on a node without the ready label.
- The pod sandbox (pause) container runs as a normal process. Each app container runs in its own VM.
- Deleting a running pod stops its VMs. No VM process remains on the node.
- An agent-sandbox `Sandbox` whose pod template sets `runtimeClassName: sbxr` starts and runs its workload.
- After `kubectl delete -f sbxr-install.yaml`, the nodes have no `sbxr` binaries, containerd config entries, or labels.

### Kubernetes, phase 2

- All phase 1 criteria still pass after the node upgrade, with unchanged pod specs.
- A pod with two containers runs one VM. The containers reach each other on `localhost`.
- The pod IP is the guest's IP. TCP, UDP, and ICMP to the pod IP work.
- `kubectl exec` runs a command in a container. Exec probes work.
- Deleting a pod sends SIGTERM to its containers, waits up to `terminationGracePeriodSeconds`, then stops the VM.

### Tests and CI

- `mise run test` runs all fake-VM scenarios on macOS arm64 and on Ubuntu, with no hypervisor and no network.
- `mise run test:hv` runs the same scenarios against a real VM. It skips when no hypervisor exists, and fails instead when `SBXR_REQUIRE_HV=1`.
- `mise run test:kind` runs the scenarios that declare Kubernetes support against a kind cluster on Linux x86_64.
- CI runs the jobs listed in the CI contract, and every job uses the same `mise` tasks as a local run.

## Implementation Decisions

### Platforms and dependencies

- Host platforms: macOS arm64 (macOS 15 or later), Linux x86_64 and arm64 with `/dev/kvm`.
- Guest images: `linux/arm64` on arm64 hosts, `linux/amd64` on x86_64 hosts. `sbxr` does not emulate other architectures.
- VMM: `libkrun` 1.x, minimum 1.19.8, and its `libkrunfw` kernel bundle, minimum 5.6.4. `libkrun` 2.0 is not supported until it has a stable release.
- `sbxr` loads `libkrun` at runtime with `purego`. The binary builds with cgo off.
- `libkrun` has no runtime version query. `sbxr` validates `libkrun` by checking that every required symbol exists, and validates `libkrunfw` by its reported version. A missing symbol or an old `libkrunfw` is a preflight failure.
- Library location: `SBXR_LIB_DIR` when set, otherwise the platform default (`/opt/homebrew/lib` on macOS, the system library path on Linux). The directory must contain both `libkrun` and `libkrunfw`, because `libkrun` loads `libkrunfw` by name.
- macOS: the `sbxr` binary is signed with the `com.apple.security.hypervisor` entitlement. Every build, including test binaries, is signed. An unsigned binary fails the `entitlement` check instead of failing at VM start.
- Linux: `sbxr` runs without root. It needs `newuidmap` and `newgidmap` (package `uidmap`), a subordinate uid and gid range for the user, and unprivileged user namespaces allowed. On Ubuntu 23.10 and later, AppArmor restricts unprivileged user namespaces, and `sbxr` needs an AppArmor profile (see Q3).
- CLI stack: cobra with `charm.land/fang/v2` 2.0.1 or later, logging with `charm.land/log/v2`, styles with `charm.land/lipgloss/v2`.

### Process model

- One VM equals one OS process, because `libkrun` takes over the process that starts the VM and exits with the workload's exit code.
- The CLI starts the VM in a child process (a hidden mode of the same `sbxr` binary). The child is started with the library search path already set, because the dynamic loader reads it only at process start.
- On Linux, the child runs inside a new user namespace with the user's subordinate id range mapped. The image unpack for that run uses the same mapping.
- The CLI process forwards SIGINT, SIGTERM, and terminal size to the child, and reports the child's exit code. A signal stops the VM at once (no graceful guest shutdown in the CLI). The CLI then exits 130 or 143.
- `libkrun` aborts the process on some invalid inputs instead of returning an error. `sbxr` validates every value before it calls `libkrun`, so an abort is always an `sbxr` bug.

### Guest model (CLI and Kubernetes phase 1)

- The host pulls the image, unpacks it into the cache, and shares the root filesystem with the guest over virtio-fs. The exit code comes back through virtio-fs, so the root must be virtio-fs.
- File ownership:
  - On macOS, image ownership and modes are stored as `user.containers.override_stat` extended attributes, which `libkrun` reads.
  - On Linux, the unpack runs inside the user namespace, so files get real mapped ownership.
- The unpacker applies OCI whiteouts itself, keeps hardlinks, and skips device nodes (the guest mounts its own `/dev`).
- The workload runs under the `libkrun` built-in init. No `sbxr` agent runs in the guest.
- The workload command, env, and working directory reach the guest through a config file in the guest root, not through the kernel command line, because the kernel command line breaks on quotes and non-ASCII text.
- The env list passed to `libkrun` is always explicit. An empty list stays empty. `sbxr` never lets `libkrun` copy the host env.

### CLI contract

Commands:

| Command                                 | Purpose                                                                                 |
| --------------------------------------- | --------------------------------------------------------------------------------------- |
| `sbxr run [flags] IMAGE [CMD [ARG...]]` | Run a workload in a new VM, in the foreground, and remove it after exit                 |
| `sbxr doctor [--json]`                  | Check the machine and print fix commands                                                |
| `sbxr prune [--force]`                  | Remove cached images. Asks for confirmation on a terminal. Requires `--force` otherwise |
| `sbxr completion SHELL`                 | Print shell completion (from fang)                                                      |
| `sbxr --version`                        | Print version (from fang)                                                               |

`sbxr` without arguments prints a short usage text with an example `run` command.

`run` flags:

| Flag                            | Default     | Meaning                                                           |
| ------------------------------- | ----------- | ----------------------------------------------------------------- |
| `--cpus N`                      | 1           | vCPUs, 1 to host CPU count                                        |
| `--memory SIZE`                 | `512M`      | Guest RAM, for example `512M`, `2G`                               |
| `-e, --env KEY=VALUE`           | none        | Set an env var. Repeatable                                        |
| `--env-file PATH`               | none        | Read env vars from a file                                         |
| `-v, --volume HOST:GUEST[:ro]`  | none        | Share a host directory. Repeatable                                |
| `--net`                         | off         | Give the workload IPv4 TCP and UDP client access through the host |
| `-p, --publish HOST:GUEST`      | none        | Publish a guest TCP port. Requires `--net`. Repeatable            |
| `-w, --workdir PATH`            | image value | Working directory in the guest                                    |
| `-u, --user UID[:GID]`          | image value | User in the guest                                                 |
| `--entrypoint CMD`              | image value | Replace the image entrypoint                                      |
| `--pull missing\|always\|never` | `missing`   | When to download the image                                        |
| `-i, --interactive`             | off         | Keep stdin open                                                   |
| `-t, --tty`                     | off         | Allocate a terminal                                               |
| `-q, --quiet`                   | off         | Hide progress output                                              |

Global flags: `--debug`, `--no-color`.

Flags after `IMAGE` belong to the workload. A `--` separator is optional.

Environment variables:

| Variable         | Meaning                                                                |
| ---------------- | ---------------------------------------------------------------------- |
| `SBXR_LIB_DIR`   | Directory with `libkrun` and `libkrunfw`                               |
| `SBXR_CACHE_DIR` | Cache location. Default: the platform user cache directory plus `sbxr` |
| `SBXR_LOG_LEVEL` | `debug`, `info`, `warn`, `error`. Default `warn`                       |
| `NO_COLOR`       | Disable colors when set and not empty                                  |

Configuration precedence: flags, then environment variables, then defaults. v1 has no configuration file.

Exit codes for `sbxr run`:

| Code          | Meaning                                                     |
| ------------- | ----------------------------------------------------------- |
| workload code | The workload ran and exited with this code                  |
| 125           | `sbxr` failed: invalid input, preflight, image, or VM setup |
| 126           | The command exists in the image but cannot run              |
| 127           | The command does not exist in the image                     |
| 130           | Interrupted by SIGINT                                       |
| 143           | Stopped by SIGTERM                                          |

A workload that exits 125, 126, or 127 cannot be told apart from an `sbxr` failure by exit code. `sbxr` failures always print a message that starts with `sbxr:` on stderr. Other commands exit 0 on success, 1 on failure, and 2 on usage errors.

Output streams:

- stdout carries only workload output (`run`) or the command result (`doctor --json`).
- stderr carries logs, progress, warnings, and errors.
- Progress shows only when stderr is a terminal. `run` prints nothing of its own on success unless `--debug` is set.
- Errors are one plain line, `sbxr: <message>`, colored only on a terminal.

`sbxr doctor` contract:

| Check ID      | Platform | Passes when                                                                                       |
| ------------- | -------- | ------------------------------------------------------------------------------------------------- |
| `hypervisor`  | all      | `kern.hv_support` is 1 (macOS), or `/dev/kvm` opens read-write and reports API version 12 (Linux) |
| `libkrun`     | all      | The library loads and exports every required symbol                                               |
| `libkrunfw`   | all      | The library loads and reports version 5.6.4 or later                                              |
| `entitlement` | macOS    | The running binary has the hypervisor entitlement                                                 |
| `userns`      | Linux    | A user namespace with the user's subordinate ids can be created                                   |
| `cache`       | all      | The cache directory is writable                                                                   |

`sbxr doctor --json` output shape:

```json
{
  "schemaVersion": 1,
  "ok": false,
  "checks": [
    { "id": "hypervisor", "status": "pass", "detail": "Hypervisor.framework available" },
    {
      "id": "libkrun",
      "status": "fail",
      "detail": "libkrun not found in /opt/homebrew/lib",
      "fix": "brew install libkrun"
    }
  ]
}
```

`status` is `pass` or `fail`. `fix` is present only when `status` is `fail`.

### Networking (CLI)

- Default: no network. Socket forwarding (TSI) is turned off, and the guest has no network path.
- `--net`: the workload opens connections as the host process does, through `libkrun` socket forwarding (TSI). This includes host `localhost` and the LAN. Help text states this.
- `--net` supports IPv4 TCP and UDP client traffic. It does not support IPv6, ICMP, raw sockets, or UDP listeners. `sbxr` writes the host's DNS resolver settings into the guest, so names resolve.
- `-p`: `sbxr` always passes an explicit port list. It never exposes guest ports that the user did not list.

### Run lifecycle

States for one run: `validating` -> `pulling` -> `preparing` -> `running` -> `exited` -> `cleaned`.

- Any failure before `running` goes to `cleaned` with exit 125.
- A signal in any state goes to `cleaned` with exit 130 or 143.
- `cleaned` is reached on every path. Run state never outlives the `sbxr run` process.
- Concurrent runs share the image cache. Writes to the cache are locked. Each run has its own state.
- On Linux, cache files belong to mapped subordinate ids. `sbxr prune` removes them inside the user namespace.

### Kubernetes contract, both phases

- RuntimeClass name and handler: `sbxr`.
- RuntimeClass `scheduling.nodeSelector`: `sbxr.dev/ready: "true"`.
- RuntimeClass `overhead.podFixed`: memory for the guest kernel and VMM. The value is set after measurement.
- Installer: one DaemonSet manifest, published as release asset `sbxr-install.yaml`. The DaemonSet runs preflight on the node, installs binaries and libraries, adds the containerd runtime entry for handler `sbxr`, restarts containerd, and sets the node label. A failed preflight leaves the node unlabeled and logs the reason.
- Uninstall: release asset `sbxr-uninstall.yaml`.
- Supported node runtime: containerd 2.x.
- containerd pulls the container image, not `sbxr`.

### Kubernetes phase 1: crun with libkrun

- The installer ships `crun` built with `libkrun` support, plus `libkrun` and `libkrunfw`. No distribution package provides this build.
- containerd entry for handler `sbxr`: runtime type `io.containerd.runc.v2`, with the binary set to a `krun`-named link to that `crun`.
- `crun` runs the pod sandbox container as a normal process and each app container and init container in its own VM.
- VM memory comes from the container memory limit. VM vCPUs come from the container CPU affinity. Defaults: 1 vCPU and 1024 MiB.
- Networking uses TSI inside the pod network namespace. TCP works. ICMP, IPv6, and UDP listeners do not.
- Known gaps: no `kubectl exec`, no exec probes, no graceful stop (pod deletion stops the VM at once), and one VM per container.

### Kubernetes phase 2: sbxr runtime

- containerd runtime type `io.containerd.sbxr.v1`, shim binary `containerd-shim-sbxr-v1`. containerd uses the shim as the pod sandboxer (containerd sandbox API), so the pod has no pause container.
- One VM per pod. The shim process is the VM process.
- The guest boots the `sbxr` guest agent as PID 1 instead of the `libkrun` built-in init.
- Guest agent responsibilities:
  - Mount system filesystems, reap processes, set hostname and pod networking.
  - Mount each container's root filesystem and volumes.
  - Create and start the pod's containers with `crun` inside the guest, in shared pod namespaces.
  - Run exec processes and resize terminals.
  - Carry stdin, stdout, and stderr as separate streams.
  - Forward signals, report exit codes and events, and run graceful stop: SIGTERM, grace period, SIGKILL, guest power-off.
- Host-guest protocol: TTRPC with the containerd task API on one vsock control port, plus one vsock port for raw stdio streams.
- Pod networking: a TAP device in the pod network namespace, redirected to the pod interface, so the guest owns the pod IP. This needs `libkrun` built with network support.

### Test contract

- Behavior scenarios are written once and run against three targets: `fake` (no hypervisor), `cli` (real VM), and `k8s` (kind cluster).
- Each scenario declares the capabilities it needs, for example `stdin`, `tty`, `split-stderr`, `volumes`, `ports`, `exec`. A target runs only the scenarios that match its capabilities.
- Scenarios assert on behavior: stdout, exit code, and a target-neutral error class. Error classes: `ok`, `invalid-spec`, `image`, `preflight`, `not-found`, `not-executable`, `interrupted`.
- After each scenario, the target verifies that no VM process, pod, or run state remains.
- Hypervisor detection uses the same rules as the `hypervisor` doctor check.
- `SBXR_REQUIRE_HV=1` turns hypervisor skips into failures. CI sets it on hypervisor jobs.
- On macOS, every test binary that starts a real VM is signed before it runs.

CI contract:

| Job         | Runner                       | Runs                                                               |
| ----------- | ---------------------------- | ------------------------------------------------------------------ |
| check       | `ubuntu-latest`              | lint, unit tests, fake tier                                        |
| linux-arm64 | `ubuntu-24.04-arm`           | fake tier (the runner has no KVM)                                  |
| macos       | `macos-15`                   | fake tier, codesign check, `doctor` reports the missing hypervisor |
| hv          | `ubuntu-latest`, KVM enabled | real-VM tier, `SBXR_REQUIRE_HV=1`                                  |
| kind        | `ubuntu-latest`, KVM enabled | Kubernetes tier                                                    |

- KVM on `ubuntu-latest` needs a udev rule that opens `/dev/kvm` to all users. kind resets `/dev/kvm` permissions when it creates a cluster, so the kind job sets them again after creation.
- CI builds `libkrun` from source at the pinned version, uses the `libkrunfw` release tarball, and caches both by version.
- Real-VM tests on macOS and Linux arm64 run locally or on a self-hosted runner, because hosted macOS and arm64 runners have no usable hypervisor.

### Repository and tooling

- Project base: `albertsko/go-kickstart` (mise tools and tasks, strict golangci-lint, goreleaser).
- `vendor/` is gitignored. `mise run deps` runs `go mod vendor`. CI runs the same task, so local and CI builds use the same vendored sources.
- Tasks: `test`, `test:hv`, `test:kind`, `lint`, `build`, `ci`. The same tasks run locally and in CI.

## Out of Scope

- Persistent, named, or background VMs (`start`, `stop`, `ls`, `exec` into a running VM).
- Snapshots, checkpoints, branching, pause and resume.
- Packing VMs into images or self-running executables.
- Building images. `sbxr` runs existing OCI images only.
- Device nodes from image layers.
- IPv6 and ICMP in the CLI.
- GPU access, display, or audio in the guest.
- Egress allow-lists, credential injection, and other network policy.
- Windows and Intel macOS hosts.
- Emulating a different CPU architecture.
- Confidential computing variants (`libkrun-sev`, `libkrun-tdx`).
- An HTTP API, a web interface, or a TUI. The core stays ready for them.
- A configuration file in v1.
- `kubectl attach` and `kubectl port-forward` in phase 1.
- Custom Kubernetes resources. agent-sandbox covers that need.
- Language SDKs.

## Assumptions and Open Questions

### Assumptions

| ID  | Assumption                                                                                                                                                         | Confidence |
| --- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ---------- |
| A1  | `libkrun` 1.19.x stays maintained while 2.0 matures                                                                                                                | 75%        |
| A2  | The `libkrun` built-in init reads the workload command, env, and working directory from a config file in the guest root, as `crun` uses it                         | 75%        |
| A3  | One scenario set with capability tags covers the CLI and Kubernetes without target-specific branches inside scenarios                                              | 60%        |
| A4  | An AppArmor profile for the `sbxr` binary allows unprivileged user namespaces on Ubuntu without changing the system-wide setting                                   | 70%        |
| A5  | `crun` with `libkrun` runs inside kind nodes with `/dev/kvm` passed through                                                                                        | 70%        |
| A6  | In phase 1, `kubectl logs` shows workload output. stderr may be merged into stdout                                                                                 | 60%        |
| A7  | fang issue 104 gets fixed upstream (open pull requests 106 and 108). Until then, help and usage errors wait 4 seconds on terminals that never answer color queries | 60%        |
| A8  | `libkrun` built with network support provides TAP networking for phase 2 on Linux                                                                                  | 80%        |
| A9  | TCP probes and port-forward work in phase 1 through TSI in the pod network namespace                                                                               | 55%        |

### Open questions

- **Q1. Kubernetes distributions.** Which containerd config layouts the installer supports first (kind, kubeadm, k3s, RKE2, managed clouds).
- **Q2. Distribution.** Homebrew tap name, Linux packages, and whether macOS release binaries get a Developer ID signature and notarization.
- **Q3. Ubuntu user namespaces.** Ship an AppArmor profile with the Linux packages, or document the system-wide setting change. Recommendation: ship a profile, if A4 holds.
- **Q4. Default resources.** Confirm 1 vCPU and 512 MiB for the CLI after measuring workloads. Boot measurement so far: about 0.1 s and 119 MiB peak host memory on Apple silicon, about 0.5 s on GitHub `ubuntu-latest`.
- **Q5. fang issue 104.** Accept the delay until upstream fixes it, or pin a fork with the fix.

## Further Notes

- **Prior art.** `containerd/nerdbox` (v0.2.7) is a containerd shim on `libkrun` loaded through `purego`, with its own kernel, guest agent, and EROFS root. It runs one VM per container and documents no Kubernetes setup. `smol-machines/smolvm` (v1.34.3) is a full machine CLI on forked `libkrun`, with many features that `sbxr` leaves out on purpose.
- **Why `libkrun` 1.x.** The `libkrun` main branch is the 2.0 API, which is not compatible and not released. 1.19.8 (2026-11-06) is the latest stable release and is in homebrew-core.
- **Why phase 1 uses crun.** `crun` already runs `libkrun` VMs in Kubernetes-style containers with no new code. A correct one-VM-per-pod runtime needs a guest agent, the containerd sandbox API, and TAP networking, which is phase 2.
- **Security of `--net`.** Socket forwarding makes guest connections look like host connections. This is why network access is off by default.
- **Graceful stop.** `libkrun` 1.x offers an orderly shutdown signal only on macOS arm64. A signal to the VM process stops the VM at once. Only the phase 2 guest agent gives workloads a graceful stop.
- **Exit code 125.** `sbxr` follows the Docker exit-code convention. The `libkrun` init also uses 125 for setup failures inside the guest. Both are `sbxr` failures from the user's view, and the stderr message says which.
- **Why not a block-device root.** A block-device root carries ownership without a user namespace, but the guest exit code then always reads 0, because `libkrun` returns the exit code only through a virtio-fs root.
- **Verification log (2026-11-13).**
  - A `purego` program with cgo off booted Alpine through `libkrun` 1.19.8 on Apple silicon. Exit codes 0, 3, 126, and 127 passed through. Ownership xattrs worked. Unsigned binaries fail at VM creation.
  - GitHub `ubuntu-latest` and `ubuntu-24.04` (x86_64) expose KVM after a udev rule. `ubuntu-24.04-arm`, `macos-15`, and `macos-26` runners have no usable hypervisor. The `macos-14` label is retired.
  - A rootless user-namespace run on `ubuntu-latest` showed correct guest ownership (`/etc/shadow` 0:42), kept setuid bits, and passed through the exit code.
  - kind 0.33 nodes reach `/dev/kvm` after the permission reset.
  - `charm.land/fang/v2` 2.0.1 builds with `charm.land/lipgloss/v2` 2.1.0 and with `vendor/`.
- **Council review.** Four independent reviews (architecture, VMM and security, CLI experience, tests and CI) shaped these decisions: network off by default, explicit port and env lists, capability-tagged scenarios, signing test binaries, `libkrunfw` as a dependency, and the phase split.
