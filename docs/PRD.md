# sbxr: Product Requirements

Status: draft, written on the go. Last update: 2026-11-13.

## Problem Statement

Developers and platform teams want to run untrusted or experimental code, such as AI agent tools, CI jobs, or third-party images, with stronger isolation than a container gives. A container shares the host kernel. A virtual machine does not, but today a VM costs too much effort:

- Machine users must learn a large tool, install several helpers, and write configuration before they can run one command in isolation.
- Cluster users must install heavy runtimes (Kata, KubeVirt) or experimental projects that do not document Kubernetes use.
- Tools for the laptop and tools for the cluster behave differently, so a workload tested on a laptop can fail in the cluster.
- When a setup is broken (no hypervisor, missing library, missing macOS entitlement), tools often fail late with unclear errors.

## Solution

`sbxr` runs an OCI container image as a small virtual machine with its own Linux kernel, using `libkrun`. It has two front doors over one shared core:

- **Machine:** a CLI. `sbxr run alpine echo hi` behaves like `docker run`, but the workload runs in a microVM. It works on macOS arm64 (Hypervisor.framework) and Linux (KVM).
- **Kubernetes:** a RuntimeClass named `sbxr`. A pod opts in with `runtimeClassName: sbxr`. One manifest installs it on the nodes.

`sbxr` stays minimal. It runs a workload, streams its output, returns its exit code, and cleans up. It checks the environment before it starts any VM and stops with a clear fix hint when something is wrong.

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

### Cluster user (Kubernetes)

20. As a cluster admin, I want to install `sbxr` on nodes with one `kubectl apply`, so that I do not have to edit containerd config by hand.
21. As a cluster admin, I want only nodes that pass the `sbxr` checks to receive `sbxr` pods, so that pods never land on a node without a hypervisor.
22. As a cluster admin, I want one manifest to uninstall `sbxr`, so that I can remove it cleanly.
23. As an application developer, I want to opt in with `runtimeClassName: sbxr` only, so that I do not change my images or the rest of my pod spec.
24. As an application developer, I want `kubectl logs` to show workload output and the pod status to show the exit code, so that I debug the same way as with normal pods.
25. As an application developer, I want pod deletion to stop the VM within the grace period, so that rollouts and scale-down work.
26. As a platform team, I want `kubernetes-sigs/agent-sandbox` resources to work with `sbxr` as their runtime class, so that we can manage sandboxes with a standard API.

### Maintainer and contributor

27. As a maintainer, I want one command to run the same tests on macOS arm64 and on Ubuntu, so that local results match CI.
28. As a maintainer, I want behavior scenarios that run against a fake VM, a real CLI, and a kind cluster, so that the CLI and Kubernetes keep the same behavior.
29. As a maintainer, I want CI to fail when a hypervisor is expected but missing, so that VM tests are never skipped silently.
30. As an AI agent working on the code, I want dependency sources in a local `vendor/` directory, so that I can read them without network access.
31. As a maintainer, I want the core to have no CLI or Kubernetes code in it, so that we can add a web or TUI front door later.

## Acceptance Criteria

### Install and preflight

- After `brew install` on macOS arm64, `sbxr doctor` exits 0 on a machine with Hypervisor.framework support.
- `sbxr doctor` checks: hypervisor available, `libkrun` loadable with all required symbols, `libkrunfw` loadable, hypervisor entitlement present on the running binary (macOS only), cache directory writable.
- When any check fails, `sbxr doctor` exits 1 and prints one line per failed check with a fix command.
- `sbxr run` runs the same checks first. When a check fails, `sbxr run` exits 125 and no VM process starts.

### Run a workload (CLI)

- `sbxr run alpine echo hi` prints `hi` on stdout, prints nothing else on stdout, and exits 0.
- `sbxr run alpine sh -c 'exit 3'` exits 3.
- A command that does not exist in the image exits 127. A file that cannot be executed exits 126.
- The image `Entrypoint`, `Cmd`, `Env`, `WorkingDir`, and `User` apply. Arguments after the image replace `Cmd`. `-e` overrides image `Env`.
- The host environment never leaks into the guest. Only image env and `-e` / `--env-file` values are set.
- `--memory 0`, `--cpus 0`, more CPUs than the host has, an invalid image reference, or a malformed `-p` / `-v` value fail with exit 125 before any download or VM start.
- A missing image fails with exit 125, names the image, and leaves no partial cache entry.
- An image without a variant for the host architecture fails with exit 125 and names the available platforms.
- An image layer with paths that differ only by letter case fails with exit 125 on a case-insensitive host filesystem.
- After every run, including failed and interrupted runs, no VM process and no run state remain.

### Signals and interactive use

- Ctrl-C during `sbxr run` stops the VM and exits 130.
- SIGTERM to `sbxr run` stops the VM and exits 143.
- `sbxr run -it alpine sh` gives an interactive shell. Terminal resize reaches the guest.

### Networking and mounts

- Without `--net`, the workload cannot open any outbound connection.
- With `--net`, the workload reaches a server on the host network.
- `-p 8080:80` without `--net` fails with exit 125 before any VM start.
- `-p 8080:80` with `--net` makes the guest service reachable on host port 8080, and no other guest port is reachable.
- `-v ./dir:/work` makes host files visible and writable at `/work`. `-v ./dir:/work:ro` rejects writes.

### Output

- When stdout is not a terminal, or `NO_COLOR` is set, or `--no-color` is passed, output contains no ANSI escape codes.
- `sbxr` messages on stderr start with `sbxr:` when output is plain.
- `sbxr` help and error output renders in under 1 second on a terminal that does not answer color queries.

### Kubernetes

- After `kubectl apply -f sbxr-install.yaml`, every node that passes preflight gets the label `sbxr.dev/ready=true`, and the RuntimeClass `sbxr` exists.
- A pod with `runtimeClassName: sbxr` and `restartPolicy: Never` that runs `echo hi` reaches phase `Succeeded`, and `kubectl logs` shows `hi`.
- A pod whose container exits 3 reports `exitCode: 3` in its container status.
- A pod with `runtimeClassName: sbxr` is never scheduled on a node without the ready label.
- Deleting a running pod stops its VM within `terminationGracePeriodSeconds`. No VM process remains on the node.
- An agent-sandbox `Sandbox` whose pod template sets `runtimeClassName: sbxr` starts and runs its workload.
- After `kubectl delete -f sbxr-install.yaml`, the nodes have no `sbxr` binaries, containerd config entries, or labels.

### Tests and CI

- `mise run test` runs all fake-VM scenarios on macOS arm64 and on Ubuntu, with no hypervisor and no network.
- `mise run test:hv` runs the same scenarios against a real VM. It skips when no hypervisor exists, and fails instead when `SBXR_REQUIRE_HV=1`.
- `mise run test:kind` runs the scenarios that declare Kubernetes support against a kind cluster on Linux.
- CI runs the fake tier on Ubuntu and macOS, the real-VM tier on Ubuntu x86_64, and the kind tier on Ubuntu x86_64.

## Implementation Decisions

### Platforms and dependencies

- Host platforms: macOS arm64 (macOS 14 or later), Linux x86_64 and arm64 with `/dev/kvm`.
- Guest images: `linux/arm64` on macOS arm64 and Linux arm64, `linux/amd64` on Linux x86_64. `sbxr` does not emulate other architectures.
- VMM: `libkrun` 1.x, minimum 1.19.8, and its `libkrunfw` kernel bundle. `libkrun` 2.0 is not supported until it has a stable release.
- `sbxr` loads `libkrun` at runtime with `purego`. The binary builds with cgo off.
- `sbxr` checks all required `libkrun` symbols when it loads the library. A missing symbol is a preflight failure, not a crash later.
- Library lookup order: `SBXR_LIBKRUN` (full path), then the platform default paths (`/opt/homebrew/lib` on macOS, the system library path on Linux).
- macOS: the `sbxr` binary is signed with the `com.apple.security.hypervisor` entitlement. Every build, including test binaries, is signed.
- CLI stack: cobra with `charm.land/fang/v2`, logging with `charm.land/log/v2`, styles with `charm.land/lipgloss/v2`.

### Process model

- One VM equals one OS process, because `libkrun` takes over the process that starts the VM and exits with the workload's exit code.
- The CLI starts the VM in a child process (a hidden mode of the same `sbxr` binary). The CLI process forwards signals and terminal size to the child and reports the child's exit code.
- In Kubernetes, the containerd shim process is the VM process. There is no extra process between them.

### Guest model

- The host pulls the image, unpacks it into the cache, and shares the root filesystem with the guest over virtio-fs.
- On macOS, file ownership and modes from the image are stored as extended attributes that `libkrun` reads.
- CLI workloads run under the `libkrun` built-in init. No `sbxr` agent runs in the guest for the CLI.
- The Kubernetes runtime needs a small `sbxr` guest agent for graceful stop and pod lifecycle. See open questions Q1 and Q2.

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

| Flag                            | Default     | Meaning                                                |
| ------------------------------- | ----------- | ------------------------------------------------------ |
| `--cpus N`                      | 1           | vCPUs, 1 to host CPU count                             |
| `--memory SIZE`                 | `512M`      | Guest RAM, for example `512M`, `2G`                    |
| `-e, --env KEY=VALUE`           | none        | Set an env var. Repeatable                             |
| `--env-file PATH`               | none        | Read env vars from a file                              |
| `-v, --volume HOST:GUEST[:ro]`  | none        | Share a host directory. Repeatable                     |
| `--net`                         | off         | Give the workload network access through the host      |
| `-p, --publish HOST:GUEST`      | none        | Publish a guest TCP port. Requires `--net`. Repeatable |
| `-w, --workdir PATH`            | image value | Working directory in the guest                         |
| `-u, --user UID[:GID]`          | image value | User in the guest                                      |
| `--entrypoint CMD`              | image value | Replace the image entrypoint                           |
| `--pull missing\|always\|never` | `missing`   | When to download the image                             |
| `-i, --interactive`             | off         | Keep stdin open                                        |
| `-t, --tty`                     | off         | Allocate a terminal                                    |
| `-q, --quiet`                   | off         | Hide progress output                                   |

Global flags: `--debug`, `--no-color`.

Flags after `IMAGE` belong to the workload. A `--` separator is optional.

Environment variables:

| Variable         | Meaning                                                                |
| ---------------- | ---------------------------------------------------------------------- |
| `SBXR_LIBKRUN`   | Full path to the `libkrun` library                                     |
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
      "detail": "libkrun not found",
      "fix": "brew install libkrun"
    }
  ]
}
```

Check IDs: `hypervisor`, `libkrun`, `libkrunfw`, `entitlement` (macOS only), `cache`. `status` is `pass` or `fail`. `fix` is present only when `status` is `fail`.

### Networking

- Default: no network. The VM has no network device and socket forwarding is off.
- `--net`: the workload opens connections as the host process does, through `libkrun` socket forwarding (TSI). This includes host `localhost` and the LAN. Help text states this.
- `-p`: `sbxr` always passes an explicit port list. It never exposes guest ports that the user did not list.

### Run lifecycle

States for one run: `validating` -> `pulling` -> `preparing` -> `running` -> `exited` -> `cleaned`.

- Any failure before `running` goes to `cleaned` with exit 125.
- A signal in any state goes to `cleaned` with exit 130 or 143.
- `cleaned` is reached on every path. Run state never outlives the `sbxr run` process.
- Concurrent runs share the image cache. Writes to the cache are locked. Each run has its own state.

### Kubernetes contract

- containerd runtime type: `io.containerd.sbxr.v1`. Shim binary: `containerd-shim-sbxr-v1`.
- RuntimeClass name and handler: `sbxr`.
- RuntimeClass `scheduling.nodeSelector`: `sbxr.dev/ready: "true"`.
- RuntimeClass `overhead.podFixed`: memory for the guest kernel and VMM. The value is set after measurement.
- Installer: one DaemonSet manifest, published as a release asset `sbxr-install.yaml`. The DaemonSet runs preflight on the node, installs the shim and libraries, adds the containerd runtime entry, restarts containerd, and sets the node label. A failed preflight leaves the node unlabeled and logs the reason.
- Supported node runtimes: containerd 2.x. Supported distributions are listed after testing (see Q4).
- Uninstall: release asset `sbxr-uninstall.yaml`.
- The container image is pulled by containerd, not by `sbxr`.

### Test contract

- Behavior scenarios are written once and run against three targets: `fake` (no hypervisor), `cli` (real VM), and `k8s` (kind cluster).
- Each scenario declares the capabilities it needs, for example `stdin`, `tty`, `split-stderr`, `volumes`, `ports`. A target runs only the scenarios that match its capabilities.
- Scenarios assert on behavior: stdout, exit code, and a target-neutral error class. Error classes: `ok`, `invalid-spec`, `image`, `preflight`, `not-found`, `not-executable`, `interrupted`.
- After each scenario, the target verifies that no VM process, pod, or run state remains.
- Hypervisor detection: `kern.hv_support` on macOS, read-write open of `/dev/kvm` on Linux.
- `SBXR_REQUIRE_HV=1` turns hypervisor skips into failures. CI sets it on hypervisor jobs.

CI jobs:

| Job   | Runner                                | Runs                                                                 |
| ----- | ------------------------------------- | -------------------------------------------------------------------- |
| check | ubuntu-latest                         | lint, unit tests, fake tier                                          |
| macos | macos-15 arm64                        | fake tier, codesign smoke test, preflight reports missing hypervisor |
| hv    | ubuntu-latest x86_64 with KVM enabled | real-VM tier, `SBXR_REQUIRE_HV=1`                                    |
| kind  | ubuntu-latest x86_64 with KVM enabled | Kubernetes tier                                                      |

`libkrun` and `libkrunfw` are built once per version in CI and cached. Real-VM tests on macOS run locally and on a release checklist, because hosted macOS runners have no nested virtualization.

### Repository and tooling

- Project base: `albertsko/go-kickstart` (mise tools and tasks, strict golangci-lint, goreleaser).
- `vendor/` is gitignored. `mise run deps` runs `go mod vendor`. CI runs the same task, so local and CI builds use the same vendored sources.
- Tasks: `test`, `test:hv`, `test:kind`, `lint`, `build`, `ci`. The same tasks run locally and in CI.

## Out of Scope

- Persistent, named, or background VMs (`start`, `stop`, `ls`, `exec` into a running VM).
- Snapshots, checkpoints, branching, pause and resume.
- Packing VMs into images or self-running executables.
- Building images. `sbxr` runs existing OCI images only.
- GPU access, display, or audio in the guest.
- Egress allow-lists, credential injection, and other network policy.
- Windows and Intel macOS hosts.
- Emulating a different CPU architecture.
- Confidential computing variants (`libkrun-sev`, `libkrun-tdx`).
- An HTTP API, a web interface, or a TUI. The core stays ready for them.
- A configuration file in v1.
- `kubectl exec`, `kubectl attach`, and `kubectl port-forward` in the first Kubernetes release.
- Custom Kubernetes resources. agent-sandbox covers that need.
- Language SDKs.

## Assumptions and Open Questions

### Assumptions

| ID  | Assumption                                                                                                                | Confidence |
| --- | ------------------------------------------------------------------------------------------------------------------------- | ---------- |
| A1  | `libkrun` 1.19.x stays maintained while 2.0 matures                                                                       | 75%        |
| A2  | `purego` can call every `libkrun` 1.x function we need, because they use only integers, strings, and string arrays        | 85%        |
| A3  | A host-unpacked root over virtio-fs plus the `libkrun` built-in init gives correct exit codes, env, and user for CLI runs | 70%        |
| A4  | GitHub `ubuntu-latest` x86_64 runners expose KVM after the udev rule, and kind nodes can use it                           | 75%        |
| A5  | `ubuntu-24.04-arm` runners do not expose KVM                                                                              | 65%        |
| A6  | Hosted macOS runners cannot run real-VM tests                                                                             | 90%        |
| A7  | One scenario set with capability tags covers both CLI and Kubernetes without target-specific branches inside scenarios    | 60%        |
| A8  | `charm.land/fang/v2` works with the latest lipgloss v2, and its terminal color query (fang issue 104) can be avoided      | 65%        |
| A9  | A Linux host can preserve image file ownership for a virtio-fs root without running `sbxr` as root                        | 40%        |

### Open questions

- **Q1. Kubernetes delivery order.** Option A: build our own shim from the start. Option B: ship the CLI first, use crun's `krun` handler as a temporary RuntimeClass backend, and build our own shim second. The council split on this. Recommendation: B, because our shim needs a guest agent and a pod-to-VM mapping that the CLI does not need.
- **Q2. Pod-to-VM mapping.** One VM per pod (shared localhost, needs a guest agent that runs several containers) or one VM per container (simpler, breaks multi-container pods). Recommendation: one VM per pod, with the pause container handled without a VM.
- **Q3. Pod networking.** How the VM gets the pod IP: run the VMM inside the pod network namespace with socket forwarding, or attach a virtio-net device to the pod interface. Needs a prototype.
- **Q4. Kubernetes distributions.** Which containerd config layouts the installer supports first (kind, kubeadm, k3s, RKE2, managed clouds).
- **Q5. Linux file ownership.** If A9 is false, choose between requiring root on Linux, using a user namespace for unpacking, or a block-device root with a different exit-code path.
- **Q6. Distribution.** Homebrew tap name, Linux packages, and whether macOS release binaries get a Developer ID signature and notarization.
- **Q7. Default resources.** Confirm 1 vCPU and 512 MiB after measuring boot time and memory overhead.

## Further Notes

- **Prior art.** `containerd/nerdbox` (v0.2.7) is a containerd shim on `libkrun` loaded through `purego`, with its own kernel, guest agent, and EROFS root. It documents no Kubernetes setup. `smol-machines/smolvm` (v1.34.3) is a full machine CLI on forked `libkrun`, with many features that `sbxr` leaves out on purpose.
- **Why `libkrun` 1.x.** The `libkrun` main branch is the 2.0 API, which is not compatible and not released. 1.19.8 (2026-11-06) is the latest stable release and is in homebrew-core.
- **Security of `--net`.** Socket forwarding makes guest connections look like host connections. This is why network access is off by default.
- **Graceful stop on Linux.** `libkrun` 1.x offers an orderly shutdown signal only on macOS arm64. On Linux, stopping a VM without a guest agent is a hard stop. The CLI reports 130 or 143. The Kubernetes runtime needs the guest agent (Q1).
- **Exit code 125.** `sbxr` follows the Docker exit-code convention. The `libkrun` init also uses 125 for setup failures inside the guest. Both are `sbxr` failures from the user's view, and the stderr message says which.
- **Council review.** Four independent reviews (architecture, VMM and security, CLI experience, tests and CI) shaped these decisions: network off by default, explicit port and env lists, capability-tagged scenarios, signing test binaries, `libkrunfw` as a dependency, and the open questions Q1 to Q3.
