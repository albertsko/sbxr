# sbxr: Product Requirements

Status: draft, written on the go. Last update: 2026-11-14.

## Problem Statement

Developers and platform teams want to run untrusted or experimental code, such as AI agent tools, CI jobs, or third-party images, with stronger isolation than a container gives. A container shares the host kernel. A virtual machine does not, but today a VM costs too much effort:

- Machine users must learn a large tool, install several helpers, and write configuration before they can run one command in isolation.
- Cluster users must install heavy runtimes (Kata, KubeVirt) or experimental projects that do not document Kubernetes use.
- Tools for the laptop and tools for the cluster behave differently, so a workload tested on a laptop can fail in the cluster.
- When a setup is broken (no hypervisor, missing library, missing macOS entitlement, missing user namespace support), tools often fail late with unclear errors.
- Sandboxes often leak: guest services appear on the host network, or the guest can reach host-local services.

## Solution

`sbxr` runs an OCI container image as a small virtual machine with its own Linux kernel, using `libkrun`. It has two front doors:

- **Machine:** a CLI. `sbxr run alpine echo hi` behaves like `docker run`, but the workload runs in a microVM. It works on macOS arm64 (Hypervisor.framework) and Linux (KVM), without root. The workload has no network unless the user asks for it, and even then it cannot reach host-local services.
- **Kubernetes:** a RuntimeClass named `sbxr`. A pod opts in with `runtimeClassName: sbxr`. One manifest installs it on the nodes.

`sbxr` stays minimal. It runs a workload, streams its output, returns its exit code, and cleans up. It checks the environment before it starts any VM and stops with a clear fix hint when something is wrong.

Delivery has two phases:

- **Phase 1:** the CLI, plus a Kubernetes RuntimeClass backed by `crun` in its `libkrun` mode. Phase 1 Kubernetes runs one VM per container and has known limits (see "Kubernetes phase 1").
- **Phase 2:** the `sbxr` containerd runtime. It runs one VM per pod with a small `sbxr` guest agent and closes the phase 1 limits. The RuntimeClass name `sbxr` stays the same, so pod specs do not change.

## User Stories

### Machine user (CLI)

1. As a macOS developer, I want to install `sbxr` with one Homebrew command, so that I can start without building anything.
2. As a Linux developer, I want one copy-paste setup block, so that I can install `sbxr` and its system requirements in one step.
3. As a developer, I want `sbxr doctor` to check my machine and tell me the exact command to fix each problem, so that I do not debug setup issues myself.
4. As a developer, I want `sbxr run IMAGE CMD...` to work like `docker run`, so that I can reuse what I already know.
5. As a developer, I want the workload's stdout on stdout and `sbxr` messages on stderr, so that I can pipe workload output into other tools.
6. As a developer, I want `sbxr run` to exit with the workload's exit code, so that scripts and CI react to failures correctly.
7. As a developer, I want `sbxr run -it IMAGE sh` to give me an interactive shell, so that I can explore an image inside a VM.
8. As a developer, I want Ctrl-C to stop the VM and leave nothing running, so that I can trust that interrupted runs do not leak resources.
9. As a developer, I want the workload to have no network unless I ask for it, so that untrusted code cannot reach anything by default.
10. As a developer, I want `--net` to give internet access but never access to my host's local services or my LAN, so that I can install packages without exposing my machine.
11. As a developer, I want `--allow-lan` when I need a service on my LAN, so that I open that access only on purpose.
12. As a developer, I want `-p HOST:GUEST` to publish only the ports I list, on `127.0.0.1` by default, so that guest services never appear on my network by accident.
13. As a developer, I want `-v HOST:GUEST[:ro]`, so that I can share a project directory with the workload.
14. As a developer, I want `-e KEY=VALUE` and `--env-file`, so that I can pass configuration without baking it into the image.
15. As a developer, I want `--cpus` and `--memory` with useful defaults, so that common workloads such as compilers work without tuning.
16. As a developer, I want invalid flags and values rejected before any VM starts, with a one-line message that names the problem and the fix, so that I get fast, clear feedback.
17. As a developer, I want a clear error when the image does not exist or does not match my CPU architecture, so that I do not wait for a VM that cannot work.
18. As a developer, I want images cached between runs and `sbxr prune` to clear the cache, so that repeated runs are fast and disk use stays under control.
19. As a developer, I want each run to start from the clean image, so that one run cannot change what the next run sees.
20. As a developer, I want colors and progress only on a terminal, and plain output when piped or when `NO_COLOR` is set, so that logs stay readable.
21. As a developer, I want help and errors to appear at once, even in terminals that do not answer color queries, so that the tool never feels stuck.
22. As a developer, I want `--debug` to show detailed logs, so that I can report problems with useful context.
23. As a script author, I want `sbxr doctor --json`, so that automation can check readiness.
24. As a developer, I want to run several `sbxr run` commands at the same time, so that parallel jobs do not block or corrupt each other.
25. As a Linux developer, I want `sbxr` to run without root and still show correct file ownership in the guest, so that images that depend on root-owned files work.
26. As a developer, I want a cached `sbxr run alpine true` to finish in well under a second, so that I can use VMs in tight loops.

### Cluster user (Kubernetes)

27. As a cluster admin, I want to install `sbxr` on nodes with one `kubectl apply`, so that I do not have to edit containerd config by hand.
28. As a cluster admin, I want the installer to refuse nodes it cannot configure safely and to say why on the node, so that I never get a half-configured node.
29. As a cluster admin, I want only nodes that pass the `sbxr` checks to receive `sbxr` pods, so that pods never land on a node without a hypervisor.
30. As a cluster admin, I want `sbxr` pods that request host privileges to be rejected, so that the VM boundary is not bypassed.
31. As a cluster admin, I want one manifest to uninstall `sbxr`, so that I can remove it cleanly.
32. As a cluster admin, I want to move from phase 1 to phase 2 without changing pod specs, so that the upgrade is a node-level change only.
33. As an application developer, I want to opt in with `runtimeClassName: sbxr` only, so that I do not change my images or the rest of my pod spec.
34. As an application developer, I want `kubectl logs`, exit codes, TCP and HTTP probes, Services, and port-forward to work as with normal pods, so that I debug and operate the same way.
35. As an application developer, I want the phase 1 limits shown where I look (`kubectl describe runtimeclass sbxr`, admission warnings, docs), so that I am not surprised.
36. As an application developer, I want the containers of one pod to share one VM and `localhost`, so that sidecars work as in normal pods (phase 2).
37. As an application developer, I want `kubectl exec` and exec probes to work, so that I can debug and health-check workloads (phase 2).
38. As an application developer, I want pod deletion to send SIGTERM to my workload and wait for the grace period, so that rollouts shut down cleanly (phase 2).
39. As a platform team, I want `kubernetes-sigs/agent-sandbox` resources to work with `sbxr` as their runtime class, so that we can manage sandboxes with a standard API.

### Maintainer and contributor

40. As a maintainer, I want one command to run the same tests on macOS arm64 and on Ubuntu, so that local results match CI.
41. As a maintainer, I want behavior scenarios that run against a fake VM, a real CLI, and a kind cluster, so that the CLI and Kubernetes keep the same behavior.
42. As a maintainer, I want CI to fail when a hypervisor is expected but missing, so that VM tests are never skipped silently.
43. As an AI agent working on the code, I want dependency sources in a local `vendor/` directory, so that I can read them without network access.
44. As a maintainer, I want the core to have no CLI, Kubernetes, or network-stack code in it, so that we can add a web or TUI front door later and swap backends.

## Acceptance Criteria

### Install and preflight

- After `brew install albertsko/tap/sbxr` on macOS 15 arm64, `sbxr doctor` exits 0 on a machine with Hypervisor.framework support.
- On a fresh Ubuntu 24.04 machine, the documented setup block plus `sbxr doctor` exits 0.
- `sbxr doctor` runs every check in the `doctor` contract and reports each one.
- When any check fails, `sbxr doctor` exits 1, prints one line per failed check with a fix command, and ends with one combined fix script when more than one check fails.
- `sbxr run` runs the same checks first. When a check fails, `sbxr run` exits 125 and no VM process starts.
- On Ubuntu with unprivileged user namespaces restricted by AppArmor and no matching profile, `sbxr doctor` fails the `userns` check and prints the profile and install commands.
- When `sbxr` is installed outside `/usr/bin` or `/usr/local/bin` on such a system, `sbxr doctor` tells the user to install it to `/usr/local/bin` and does not print a profile for the other path.

### Run a workload (CLI)

- `sbxr run alpine echo hi` prints `hi` on stdout, prints nothing else on stdout, and exits 0.
- `sbxr run alpine sh -c 'exit 3'` exits 3.
- A command that does not exist in the image exits 127. A file that cannot be executed exits 126.
- The image `Entrypoint`, `Cmd`, `Env`, `WorkingDir`, and `User` apply. Arguments after the image replace `Cmd`. `-e` overrides image `Env`.
- Arguments with quotes, tabs, newlines, non-ASCII characters, and words such as `uid`, `Env`, or `args` reach the workload unchanged.
- An argument, env value, or working directory with a control character other than tab and newline fails with exit 125 and names the argument.
- `-e KRUN_INIT=x`, `-e HOSTNAME=x`, and an env name that is not a valid identifier fail with exit 125. Reserved names in the image env are dropped with a warning.
- `-u 1000` runs the workload as uid 1000, even when the arguments contain `uid 0`.
- The guest env contains only the image env, the `-e` and `--env-file` values, `HOME=/` and `TERM=linux` unless set, and `PATH` from the image or a default. The host env never leaks into the guest.
- `--memory 0`, `--cpus 0`, more CPUs than the host has, an invalid image reference, a relative `-w`, or a malformed `-p` or `-v` value fail with exit 125 before any download or VM start.
- Too many arguments and env entries for the guest init fail with exit 125 before any VM start.
- A missing image fails with exit 125, names the image, and leaves no partial cache entry.
- An image without a variant for the host architecture fails with exit 125 and names the available platforms.
- On macOS, an image layer with paths that differ only by letter case fails with exit 125.
- In the guest, files keep the owner, group, mode, setuid bits, and hardlinks from the image, on macOS and on Linux without root.
- A file that the workload creates, changes, or deletes is not changed in the image cache, and the next run sees the original image.
- An image that ships its own `/.krun_config.json`, including as a symlink, does not change what runs and does not cause any host file outside the run directory to change.
- After every run, including failed and interrupted runs, no VM process, no network listener, and no run state remain.
- With the image cached, `sbxr run alpine true` finishes in under 1 second on an Apple silicon Mac.

### Signals and interactive use

- Ctrl-C during `sbxr run` stops the VM and exits 130.
- SIGTERM to `sbxr run` stops the VM and exits 143.
- `sbxr run -it alpine sh` gives an interactive shell. Terminal resize reaches the guest.
- Without `-i`, the VM never reads the user's terminal.

### Networking and mounts

- Without `--net`, the workload cannot open any connection and no guest port is reachable from the host.
- With `--net`, the workload reaches public IPv4 addresses over TCP and UDP and resolves DNS names, including with busybox, musl, glibc, and Go resolvers.
- With `--net`, connections to the host's loopback addresses, `0.0.0.0`, the host's own interface addresses, link-local and cloud metadata addresses, RFC1918 and carrier-grade NAT ranges, and IPv6 destinations fail.
- With `--net --allow-lan`, the workload reaches RFC1918 addresses and the host's LAN address, but still not host loopback or cloud metadata.
- `--allow-lan` without `--net` and `-p` without `--net` fail with exit 125 before any VM start.
- `-p 8080:80` makes the guest service reachable on `127.0.0.1:8080` only. No other guest port is reachable, and nothing listens on other host addresses.
- `-p 0.0.0.0:8080:80` binds all host interfaces. An IPv6 bind address fails with exit 125.
- A guest that opens more connections than the per-run cap gets connection failures, and the `sbxr` process keeps working.
- `-v ./dir:/work` makes host files visible and writable at `/work`. `-v ./dir:/work:ro` rejects writes.

### Output

- When stdout is not a terminal, or `NO_COLOR` is set, or `--no-color` is passed, output contains no ANSI escape codes.
- `sbxr` errors are one line on stderr in the form `sbxr: <source> <value>: <problem>; <fix>`.
- `sbxr --help`, an unknown command, a bad flag, and a failed preflight each finish in under 1 second on a terminal that never answers color queries.
- A workload exit code, including a non-zero one, never prints any `sbxr` error text.

### Kubernetes, phase 1

- After `kubectl apply -f sbxr-install.yaml` on a supported distribution, every node that passes preflight gets the label `sbxr.dev/ready=true`, and the RuntimeClass `sbxr` exists.
- A node that fails preflight, or whose containerd does not import the `sbxr` drop-in directory, stays unlabeled, its containerd config stays unchanged, and the node annotation `sbxr.dev/preflight` states the reason.
- A pod with `runtimeClassName: sbxr` and `restartPolicy: Never` that runs `echo hi` reaches phase `Succeeded`, and `kubectl logs` shows `hi`.
- A pod whose container exits 3 reports `exitCode: 3` in its container status.
- A missing image reports `ErrImagePull` or `ImagePullBackOff`, as with runc.
- TCP and HTTP probes, ClusterIP Services, pod-to-pod traffic, and `kubectl port-forward` work.
- DNS lookups from Go, glibc, and musl programs work in `sbxr` pods without any change to the pod spec.
- A pod with `runtimeClassName: sbxr` is never scheduled on a node without the ready label.
- A pod with `runtimeClassName: sbxr` that requests privileged mode, host network, host PID, host IPC, or added capabilities is rejected at admission.
- A pod with `runtimeClassName: sbxr` that uses an exec probe or a grace period over 10 seconds gets an admission warning.
- The pod sandbox (pause) container runs as a normal process. Each app container runs in its own VM.
- The container memory limit sets the VM memory. A workload that exceeds it is killed with exit code 137.
- Deleting a running pod stops its VMs within the grace period. No VM process remains on the node.
- An agent-sandbox `Sandbox` whose pod template sets `runtimeClassName: sbxr` starts and runs its workload.
- After `kubectl delete -f sbxr-uninstall.yaml`, the nodes have no `sbxr` binaries, containerd drop-ins, labels, annotations, or admission policies.

### Kubernetes, phase 2

- All phase 1 criteria still pass after the node upgrade, with unchanged pod specs.
- A pod with two containers runs one VM. The containers reach each other on `localhost`.
- The pod IP is the guest's IP. TCP, UDP, and ICMP to the pod IP work, and busybox DNS works.
- `kubectl exec` runs a command in a container. Exec probes work.
- Deleting a pod sends SIGTERM to its containers, waits up to `terminationGracePeriodSeconds`, then stops the VM.
- A missing command reports a start failure distinct from a workload that exits 127.
- A VM whose network device or guest agent fails to start is stopped within the boot timeout and the container reports an error.

### Tests and CI

- `mise run test` runs all fake-VM scenarios on macOS arm64 and on Ubuntu, with no hypervisor and no external network.
- `mise run test:hv` runs the same scenarios against a real VM. It skips when no hypervisor exists, and fails instead when `SBXR_REQUIRE_HV=1`.
- `mise run test:kind` runs the scenarios that match the Kubernetes target's capabilities against a kind cluster on Linux x86_64.
- CI runs the jobs in the CI contract, and every job uses the same `mise` tasks as a local run.

## Implementation Decisions

### Platforms and dependencies

- Host platforms: macOS arm64 (macOS 15 or later), Linux x86_64 and arm64 with `/dev/kvm`.
- Guest images: `linux/arm64` on arm64 hosts, `linux/amd64` on x86_64 hosts. `sbxr` does not emulate other architectures.
- VMM: `libkrun` 1.x, minimum 1.19.8, and its `libkrunfw` kernel bundle, minimum 5.6.4. On Linux, `libkrun` must be built with network support (`NET=1`). One such build serves every Linux use; it behaves the same as a build without network support when no network device is added.
- `sbxr` loads `libkrun` at runtime with `purego`. The binary builds with cgo off.
- `sbxr` opens `libkrun` by its ABI 1 file name (`libkrun.so.1`, `libkrun.1.dylib`) as an absolute path in the library directory, never by bare name and never the unversioned link. A `libkrun` 2.x install is never loaded by mistake.
- `libkrun` has no runtime version query. `sbxr` validates `libkrun` by checking that every required symbol exists, including the network functions, and validates `libkrunfw` by its reported version. A missing symbol or an old `libkrunfw` is a preflight failure.
- Library directory: `SBXR_LIB_DIR` when set, otherwise the first of `/opt/homebrew/lib` (macOS), `/home/linuxbrew/.linuxbrew/lib`, `/usr/local/lib`, and the system library path that contains both `libkrun` and `libkrunfw`. `libkrun` loads `libkrunfw` by name, so the VM process starts with the library search path set to that directory.
- macOS signing: the `sbxr` binary is signed with hardened runtime and exactly two entitlements, `com.apple.security.hypervisor` and `com.apple.security.cs.disable-library-validation`. It never gets `allow-dyld-environment-variables` or `get-task-allow`. Every build, including test binaries, is signed. An unsigned binary fails the `entitlement` check instead of failing at VM start.
- Linux: `sbxr` runs without root. It needs read-write access to `/dev/kvm` (usually the `kvm` group), `newuidmap` and `newgidmap` (package `uidmap`), and a subordinate uid and gid range for the user. `sbxr` maps ids with those helpers, because one direct mapping cannot cover image ids such as gid 42.
- Ubuntu 23.10 and later restrict unprivileged user namespaces with AppArmor. `sbxr` needs this profile, installed once by root:

  ```text
  abi <abi/4.0>,
  include <tunables/global>

  profile sbxr /{usr/bin,usr/local/bin}/sbxr flags=(unconfined) {
    userns,

    include if exists <local/sbxr>
  }
  ```

  The profile covers only root-owned install paths. A profile for a user-writable path would let any local user bypass the restriction for any binary.

- CLI stack: cobra with `charm.land/fang/v2` 2.0.1 or later, logging with `charm.land/log/v2`, styles with `charm.land/lipgloss/v2`. In-process network stack: `github.com/containers/gvisor-tap-vsock` used as a library.

### Process model

- One VM equals one OS process, because `libkrun` takes over the process that starts the VM and exits with the workload's exit code. `libkrun` 2.0 keeps this behavior.
- The `sbxr run` process (the parent) prepares the run, hosts the network stack, forwards signals and terminal size, and reports the exit code. It starts the VM in a child process, which is a hidden mode of the same `sbxr` binary.
- On Linux, the child runs inside a new user namespace and mount namespace with the user's subordinate id range mapped. The child re-executes itself after the maps are written.
- The child's stdin is the user's terminal only with `-i`. Otherwise it is empty.
- A signal to the parent stops the VM at once (no graceful guest shutdown in the CLI). The parent then exits 130 or 143.
- `libkrun` aborts the process on some invalid inputs instead of returning an error. `sbxr` validates every value before it calls `libkrun`, so an abort is always an `sbxr` bug.
- In Kubernetes phase 2, the containerd shim process is the VM process.

### Root filesystem

- Image cache: the host pulls each image and unpacks it once into the cache. Runs never write to the cache.
- Unpacking applies OCI whiteouts, keeps hardlinks and setuid bits, and skips device nodes (the guest mounts its own `/dev`).
- File ownership:
  - On macOS, image ownership and modes are stored as `user.containers.override_stat` extended attributes (`uid:gid:mode`), which `libkrun` reads.
  - On Linux, the unpack runs inside the user namespace, so files get real mapped ownership. Cache files then belong to subordinate ids, and `sbxr prune` removes them inside the user namespace.
- Per-run root:
  - On macOS, `sbxr` clones the cached image directory with an APFS clone (about 0.1 s and a few MB for a 250 MB image).
  - On Linux, `sbxr` mounts an overlay inside the user namespace: the cached image as the lower layer, a per-run upper layer. Upper layers are never reused as lower layers.
- The guest root is the per-run root, shared over virtio-fs. The exit code comes back through virtio-fs, so the root must be virtio-fs.

### Workload config

- The workload command, env, working directory, uid, and gid reach the guest through a config file, `/.krun_config.json`, that `sbxr` writes into the per-run root. The kernel command line is not used, because it breaks on quotes and non-ASCII text and lets values override the file.
- `libkrun` call sequence: create the context, set CPUs and memory, set the root, set an empty env list, add the network device or disable socket forwarding, start. `sbxr` never calls the functions that set the exec command, working directory, or resource limits, and never passes a NULL env list or NULL port map.
- The guest init parser treats any string as a possible key and takes the first match. The writer therefore follows this contract:
  - Keys appear once each, in this order: `uid`, `gid`, `WorkingDir`, `Env`, `Entrypoint` (always empty), `args`.
  - `uid` and `gid` are non-empty decimal strings. An empty uid would run the workload as root.
  - `WorkingDir` is an absolute path.
  - Each env entry matches `NAME=value`, where NAME starts with a letter or underscore and contains only letters, digits, and underscores.
  - Env names that start with `KRUN_` and the name `HOSTNAME` are rejected from user input and dropped from the image env, because the guest init reads them as control settings.
  - `PATH` is always set, from the image or a default.
  - `args` is not empty.
  - Strings are raw UTF-8. The writer escapes only `"`, `\`, tab, and newline, and never writes `\u` escapes, because the guest init corrupts them silently. Other control characters are rejected.
  - The token count stays below the parser limit (16384). Above that limit the guest init ignores the file and starts a root shell, so `sbxr` rejects such input.
  - The file is written after removing any existing entry, with create-exclusive and no-follow semantics.
- Commands are not resolved on the host. Bare names reach the guest init, which searches the image `PATH`.
- Image user names in `User` are resolved by reading the image's `/etc/passwd` and `/etc/group` inside the cached image root. Path resolution never leaves that root. An unknown name fails with exit 125.
- The workload gets no supplementary groups. This differs from `docker run`.

### CLI contract

Commands:

| Command                                 | Purpose                                                                                 |
| --------------------------------------- | --------------------------------------------------------------------------------------- |
| `sbxr run [flags] IMAGE [CMD [ARG...]]` | Run a workload in a new VM, in the foreground, and remove it after exit                 |
| `sbxr doctor [--json]`                  | Check the machine and print fix commands                                                |
| `sbxr doctor apparmor-profile`          | Print the AppArmor profile to stdout (Linux)                                            |
| `sbxr prune [--force]`                  | Remove cached images. Asks for confirmation on a terminal. Requires `--force` otherwise |
| `sbxr completion SHELL`                 | Print shell completion (from fang)                                                      |
| `sbxr --version`                        | Print version (from fang)                                                               |

`sbxr` without arguments prints a short usage text with example `run` commands, including one with `--net`.

`run` flags:

| Flag                            | Default                       | Meaning                                                                                                   |
| ------------------------------- | ----------------------------- | --------------------------------------------------------------------------------------------------------- |
| `--cpus N`                      | 2, at most the host CPU count | vCPUs, 1 to host CPU count                                                                                |
| `--memory SIZE`                 | `1G`                          | Guest RAM, for example `512M`, `2G`                                                                       |
| `-e, --env KEY=VALUE`           | none                          | Set an env var. Repeatable. `KRUN_*` and `HOSTNAME` are rejected                                          |
| `--env-file PATH`               | none                          | Read env vars from a file. Same rules as `-e`                                                             |
| `-v, --volume HOST:GUEST[:ro]`  | none                          | Share a host directory. Repeatable                                                                        |
| `--net`                         | off                           | Outbound IPv4 to public addresses. Host-local, LAN, and metadata addresses are blocked                    |
| `--allow-lan`                   | off                           | Also allow RFC1918 addresses and the host's LAN address. Requires `--net`                                 |
| `-p, --publish [IP:]HOST:GUEST` | none                          | Publish a guest TCP port. Binds `127.0.0.1` unless an IPv4 address is given. Requires `--net`. Repeatable |
| `-w, --workdir PATH`            | image value                   | Working directory in the guest. Must be absolute                                                          |
| `-u, --user UID[:GID]`          | image value                   | User in the guest                                                                                         |
| `--entrypoint CMD`              | image value                   | Replace the image entrypoint                                                                              |
| `--pull missing\|always\|never` | `missing`                     | When to download the image                                                                                |
| `-i, --interactive`             | off                           | Keep stdin open                                                                                           |
| `-t, --tty`                     | off                           | Allocate a terminal                                                                                       |
| `-q, --quiet`                   | off                           | Hide progress output                                                                                      |

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

- stdout carries only workload output (`run`) or the command result (`doctor --json`, `doctor apparmor-profile`).
- stderr carries logs, progress, warnings, and errors.
- Progress shows only when stderr is a terminal. `run` prints nothing of its own on success unless `--debug` is set. With `--debug`, each published port is logged, for example `published 127.0.0.1:8080 -> 80/tcp`.
- Errors are one line: `sbxr: <source> <quoted value>: <problem>; <fix>`, colored only on a terminal. Values are quoted so that control characters are visible. Examples:

  ```text
  sbxr: -e "KRUN_INIT=/x": KRUN_* names are reserved by the VM init; rename the variable
  sbxr: argument 2 "a\x1bb": control character U+001B; only tab and newline are allowed
  sbxr: -w "app": must be absolute, for example /app
  sbxr: -p "8080:80": requires --net
  ```

- Help and error rendering never waits on terminal color queries. The CLI uses a fixed color scheme based on the terminal's 16 ANSI colors.

`sbxr doctor` contract:

| Check ID      | Platform | Passes when                                                                                                      |
| ------------- | -------- | ---------------------------------------------------------------------------------------------------------------- |
| `hypervisor`  | all      | `kern.hv_support` is 1 (macOS), or `/dev/kvm` opens read-write and reports API version 12 (Linux)                |
| `libkrun`     | all      | The ABI 1 library loads from the library directory and exports every required symbol                             |
| `libkrunfw`   | all      | The library loads and reports version 5.6.4 or later                                                             |
| `entitlement` | macOS    | The running binary has hardened runtime and both entitlements                                                    |
| `userns`      | Linux    | Inside a new user namespace with the user's subordinate ids, a chown to uid 0 gid 42 and a private mount succeed |
| `cache`       | all      | The cache directory is writable                                                                                  |

When the `userns` check fails and the AppArmor restriction is on, `sbxr doctor` checks the running binary's resolved path. For `/usr/bin/sbxr` or `/usr/local/bin/sbxr` it prints the profile install commands, for example `sbxr doctor apparmor-profile | sudo tee /etc/apparmor.d/sbxr && sudo apparmor_parser -r /etc/apparmor.d/sbxr`. For any other path it tells the user to install to `/usr/local/bin`.

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
      "detail": "libkrun.1.dylib not found in /opt/homebrew/lib",
      "fix": "brew install libkrun"
    }
  ]
}
```

`status` is `pass` or `fail`. `fix` is present only when `status` is `fail`.

### Networking (CLI)

- Default: no network. The VM has no network device and socket forwarding is disabled, so the guest can neither connect out nor listen on the host.
- `--net`: the guest gets a virtio-net device connected to a userspace network stack inside the `sbxr` parent process. The guest configures itself with DHCP and uses the stack's gateway for DNS. No helper binary and no socket file are involved.
- `libkrun` socket forwarding (TSI) is never used for CLI networking, because it exposes every guest listener on all host interfaces and cannot be limited.
- Egress policy, enforced by `sbxr` for every new TCP connection and UDP flow:
  - Always blocked: `0.0.0.0/8`, `127.0.0.0/8`, `169.254.0.0/16` (including cloud metadata), `224.0.0.0/4`, `240.0.0.0/4`, the gateway address except DNS and DHCP, and all non-IPv4 traffic except ARP.
  - Blocked unless `--allow-lan`: `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10`, and every address of the host's own interfaces (re-read while the run is active).
  - Allowed: all other IPv4 destinations.
- Enforcement has two layers, because the network library dials any destination it is given:
  - A frame filter between the VM and the network stack drops blocked frames before the stack sees them. Without it, guest UDP to `0.0.0.0` reached a host loopback service in testing.
  - `sbxr` builds the network stack from the library's packages and installs its own TCP and UDP forwarders, which check the destination again at dial time and count flows.
- `-p`: one forward per mapping, created by the network stack on the host. The default bind address is `127.0.0.1`. Only listed ports are reachable.
- Resource limits: each run allows at most 1024 established TCP connections and 256 UDP flows. Past the cap, new TCP connections get a reset and new UDP flows are dropped. The library alone limits only half-open TCP connections, and in testing a guest held 2000 connections, each costing one host file descriptor.
- The core defines a network backend interface. The userspace stack is one adapter used by the CLI. The phase 2 TAP network is another adapter. The core and the fake test VM do not depend on the userspace stack.

### Run lifecycle

States for one run: `validating` -> `pulling` -> `preparing` -> `running` -> `exited` -> `cleaned`.

- Any failure before `running` goes to `cleaned` with exit 125.
- A signal in any state goes to `cleaned` with exit 130 or 143.
- `cleaned` is reached on every path: the VM process is gone, the network stack and its listeners are closed, and the per-run root is removed. Run state never outlives the `sbxr run` process.
- Concurrent runs share the image cache read-only. Writes to the cache (pull, unpack, prune) are locked. Each run has its own root and state.

### Kubernetes contract, both phases

- RuntimeClass name and handler: `sbxr`.
- RuntimeClass `scheduling.nodeSelector`: `sbxr.dev/ready: "true"`.
- RuntimeClass `overhead.podFixed`: memory for the guest kernel and VMM. The value is set after measurement.
- RuntimeClass annotations: `sbxr.dev/phase` (`"1"` or `"2"`), `sbxr.dev/limits` (comma-separated list of known limits), `sbxr.dev/docs` (URL).
- Installer: one DaemonSet manifest, release asset `sbxr-install.yaml`, with the installer image pinned by digest and artifact checksums verified.
- Supported first: kubeadm-style nodes with containerd 2.2 or later and the default `conf.d` import, k3s and RKE2 with the `config-v3.toml.d` drop-in directory, and k0s 1.36 or later with `/etc/k0s/containerd.d`. kind is supported through a documented cluster config patch, set at cluster creation. Managed cloud node pools come later.
- Installer steps on each node:
  1. Detect the distribution from its systemd units.
  2. Run preflight: `/dev/kvm`, containerd version, and an `imports` entry that covers the drop-in directory.
  3. If preflight fails, change nothing, leave the node unlabeled, and write the reason to the node annotation `sbxr.dev/preflight`. The installer never edits a main containerd config or template.
  4. Install binaries and libraries under one directory, then write the `sbxr` drop-in file atomically under a host lock, and restart the containerd unit only when the content changed.
  5. Verify that containerd serves handler `sbxr` and the node reports it in its runtime handlers. Then set the ready label. If verification fails, remove the drop-in and restart again.
- Uninstall, release asset `sbxr-uninstall.yaml`: remove the label and annotation, remove the drop-in, restart containerd last, remove binaries and policies.
- containerd pulls the container image, not `sbxr`.
- Admission policies shipped with the installer, for pods with `runtimeClassName: sbxr`:
  - Validating, deny: privileged containers, host network, host PID, host IPC, added capabilities.
  - Validating, warn: exec probes and lifecycle exec hooks (phase 1), `terminationGracePeriodSeconds` over 10 (phase 1).
  - Mutating (phase 1): add the DNS option `use-vc` to the pod DNS config, keeping existing options and not adding it twice.

### Kubernetes phase 1: crun with libkrun

- The installer ships `crun` built with `libkrun` support, plus `libkrun`, `libkrunfw`, and the libraries they need that nodes may lack (for example `libjson-c`). No distribution package provides this build.
- containerd drop-in for handler `sbxr`: runtime type `io.containerd.runc.v2`, with the binary set to a `krun`-named link to that `crun`. The installer passes no `krun.*` annotations to `crun`, because they enable nested virtualization, custom kernels, and TAP devices.
- `crun` runs the pod sandbox container as a normal process and each app container and init container in its own VM.
- VM memory comes from the container memory limit, or 1024 MiB without a limit. VM vCPUs come from the container CPU affinity. A guest that uses all its memory is killed by the container cgroup (`OOMKilled`). A guest process that exceeds guest memory is killed by the guest kernel (exit 137, reason `Error`).
- Networking uses TSI inside the pod network namespace. Guest listeners are exposed in the pod network namespace, as with a normal pod. TCP, UDP clients, probes, Services, and port-forward work. ICMP, IPv6, and UDP listeners do not.
- Known limits, shown in the RuntimeClass annotations and the docs:
  - No `kubectl exec` and no exec probes ("the handler does not support exec").
  - SIGTERM does not reach the workload. Pod deletion waits the full grace period. Recommended: `terminationGracePeriodSeconds` of 5 or less.
  - `kubectl logs` shows stdout and stderr together, and the order across the two streams is not kept.
  - DNS over UDP fails for programs that use connected UDP sockets. The shipped mutating policy switches those programs to DNS over TCP. busybox `nslookup` still fails.
  - A command that does not exist or cannot run reports exit 127 or 126 with reason `Error`, not a start error. The log line `Couldn't execute '<path>' inside the vm: <reason>` names the cause.
  - The workload is not PID 1, so a signal it sends to itself can end it.
  - One VM per container.

### Kubernetes phase 2: sbxr runtime

- containerd runtime type `io.containerd.sbxr.v1`, shim binary `containerd-shim-sbxr-v1`. containerd uses the shim as the pod sandboxer (containerd sandbox API), so the pod has no pause container.
- One VM per pod. The shim process is the VM process. It runs without capabilities, as a per-pod uid or user namespace, and not dumpable, so a guest escape into one VM process cannot reach another pod's VM process.
- The guest boots the `sbxr` guest agent as PID 1 instead of the `libkrun` built-in init.
- Guest agent responsibilities:
  - Mount system filesystems, reap processes, set hostname and pod networking.
  - Mount each container's root filesystem and volumes.
  - Create and start the pod's containers with `crun` inside the guest, in shared pod namespaces.
  - Run exec processes and resize terminals.
  - Carry stdin, stdout, and stderr as separate streams.
  - Forward signals, report exit codes, start errors, and events, and run graceful stop: SIGTERM, grace period, SIGKILL, guest power-off.
- Host-guest protocol: TTRPC with the containerd task API on one vsock control port, plus one vsock port for raw stdio streams.
- Pod networking: a TAP device in the pod network namespace, created owned by the VM process's uid, with traffic redirected both ways between the pod interface and the TAP device. The pod interface's address moves to the guest, and the guest uses the pod interface's MAC address. Socket forwarding is off when a network device exists.
- Fail fast: before starting the VM, the shim opens the TAP device itself and fails if that fails, because `libkrun` reports a TAP failure only in a log line and then hangs. A boot watchdog stops the VM when the guest agent does not connect within the boot timeout.

### Test contract

- Behavior scenarios are written once and run against several targets: `fake` (no hypervisor), `cli` (real VM), `k8s-p1` (kind cluster with phase 1), and later `k8s-p2`.
- Each target declares its capabilities. Each scenario declares the capabilities it needs and runs only on targets that have them. Scenarios never check the target name.
- Capabilities: `stdin`, `tty`, `split-stderr`, `signal-exit`, `graceful-stop`, `exec`, `net`, `net-off`, `publish`, `dns`, `volumes`, `memory-limit`, `probes`.

  | Target   | Capabilities                                                                                 |
  | -------- | -------------------------------------------------------------------------------------------- |
  | `fake`   | `stdin`, `tty`, `split-stderr`, `signal-exit`, `net`, `net-off`, `publish`, `dns`, `volumes` |
  | `cli`    | same as `fake`                                                                               |
  | `k8s-p1` | `net`, `publish`, `dns`, `memory-limit`, `probes`                                            |
  | `k8s-p2` | `k8s-p1` plus `exec`, `graceful-stop`                                                        |

- Scenarios assert on behavior: stdout (or combined output), exit code, and a target-neutral error class. Error classes: `ok`, `invalid-spec`, `image`, `preflight`, `not-found`, `not-executable`, `interrupted`. Each target maps its own signals to these classes. Scenarios never assert the order across stdout and stderr.
- After each scenario, the target verifies that no VM process, network listener, pod, or run state remains.
- The fake target runs the real userspace network stack against a small simulated guest network, so port publishing, egress blocking, and cleanup are tested without a hypervisor.
- The config file writer is tested with property and fuzz tests against a copy of the guest init parser. The hypervisor job also runs the fuzz seed corpus through the real guest init.
- Hypervisor detection uses the same rules as the `hypervisor` doctor check.
- `SBXR_REQUIRE_HV=1` turns hypervisor skips into failures. CI sets it on hypervisor jobs.
- Every VM test has a hard timeout.
- On macOS, every test binary that starts a real VM is signed before it runs. CLI tests run the built binary, not in-process commands.

CI contract:

| Job            | Runner                                                   | Runs                                                                                                   | Budget |
| -------------- | -------------------------------------------------------- | ------------------------------------------------------------------------------------------------------ | ------ |
| check          | `ubuntu-latest`                                          | lint, unit tests, short fuzz, fake target                                                              | 6 min  |
| linux-arm64    | `ubuntu-24.04-arm`                                       | fake target (the runner has no KVM)                                                                    | 6 min  |
| macos          | `macos-15`                                               | fake target, codesign check, `doctor` reports the missing hypervisor, help timing on a silent terminal | 8 min  |
| hv             | `ubuntu-latest`, KVM enabled, AppArmor profile installed | `cli` target, fuzz corpus through the real init, `SBXR_REQUIRE_HV=1`                                   | 10 min |
| kind           | `ubuntu-latest`, KVM enabled                             | `k8s-p1` target                                                                                        | 10 min |
| fuzz (nightly) | `ubuntu-latest`                                          | long fuzz runs                                                                                         | 20 min |

- KVM on `ubuntu-latest` needs a udev rule that opens `/dev/kvm` to all users. kind resets `/dev/kvm` permissions when it creates a cluster, so the kind job sets them again after creation.
- The hv job installs `sbxr` to `/usr/local/bin` and the AppArmor profile, because the runner has the user namespace restriction on.
- CI builds `libkrun` from source at the pinned version with network support, uses the `libkrunfw` release tarball, and caches both by version. The kind job builds the phase 1 node bundle inside the kind node image and caches it.
- Real-VM tests on macOS and Linux arm64 run locally or on a self-hosted runner, because hosted macOS and arm64 runners have no usable hypervisor.

### Distribution

- macOS: a hand-written Homebrew formula in the tap `albertsko/homebrew-tap`. It depends on `libkrun`, installs the prebuilt release binary, and signs it with the two entitlements during install. The formula has no bottles, so Homebrew never re-signs the binary.
- Linux: `libkrun` from Homebrew on Linux, and the `sbxr` release binary installed to `/usr/local/bin`. The README and `sbxr doctor` print one setup block: install `uidmap`, add the user to the `kvm` group, install the binary, install the AppArmor profile.
- Release archives and checksums come from goreleaser. No goreleaser-generated Homebrew casks.
- `go install` works for development. `sbxr doctor` prints the signing command (macOS) and the install path rule (Linux).
- Developer ID signing and notarization come later, when `sbxr` ships outside Homebrew on macOS.

### Repository and tooling

- Project base: `albertsko/go-kickstart` (mise tools and tasks, strict golangci-lint, goreleaser).
- `vendor/` is gitignored. `mise run deps` runs `go mod vendor`. CI runs the same task, so local and CI builds use the same vendored sources.
- Tasks: `test`, `test:hv`, `test:kind`, `lint`, `build`, `ci`. The same tasks run locally and in CI.
- `libkrun` 1.x support: `sbxr` follows 1.19.x patch releases. It moves to `libkrun` 2.x when any of these happens: 2.0.0 ships with `libkrunfw` 6, `crun` or Fedora switch to ABI 2, 1.19.x gets no release for about 3 months, or 2.x fixes process takeover and Linux graceful shutdown. The `libkrun` binding lives behind one interface, so a 2.x binding can be added next to it.

## Out of Scope

- Persistent, named, or background VMs (`start`, `stop`, `ls`, `exec` into a running VM).
- Snapshots, checkpoints, branching, pause and resume.
- Packing VMs into images or self-running executables.
- Building images. `sbxr` runs existing OCI images only.
- Device nodes from image layers.
- IPv6 and ICMP in the CLI.
- Supplementary groups for the workload.
- Egress allow-lists and deny-lists beyond `--allow-lan`, credential injection, and other network policy.
- GPU access, display, or audio in the guest.
- Windows and Intel macOS hosts.
- Emulating a different CPU architecture.
- Confidential computing variants (`libkrun-sev`, `libkrun-tdx`).
- An HTTP API, a web interface, or a TUI. The core stays ready for them.
- A configuration file in v1.
- `kubectl attach` in phase 1.
- Custom Kubernetes resources. agent-sandbox covers that need.
- Managed cloud node pools (EKS, GKE, AKS) in the first installer release.
- Debian and RPM packages until Ubuntu LTS ships `libkrun`.
- Language SDKs.

## Assumptions and Open Questions

### Assumptions

| ID  | Assumption                                                                                                                             | Confidence |
| --- | -------------------------------------------------------------------------------------------------------------------------------------- | ---------- |
| A1  | `libkrun` 1.19.x stays maintained for the next 6 to 12 months                                                                          | 85%        |
| A2  | One phase 1 node bundle, built on an old glibc baseline with its own libraries, runs on all supported node distributions               | 70%        |
| A3  | One scenario set with capability tags covers all targets without target-specific branches inside scenarios                             | 75%        |
| A4  | RKE2 imports the `config-v3.toml.d` drop-in directory like k3s                                                                         | 70%        |
| A5  | Homebrew 6 tap trust adds at most one documented step to the macOS install                                                             | 70%        |
| A6  | Overlay edge cases not yet tested (hardlinks across copy-up, renaming lower directories) work over virtio-fs                           | 75%        |
| A7  | The phase 2 guest agent design (containerd task API over vsock, `crun` in the guest) works with `libkrun` 1.x and its own kernel boot  | 75%        |
| A8  | MutatingAdmissionPolicy at `admissionregistration.k8s.io/v1` is available on every Kubernetes version the installer supports           | 75%        |
| A9  | The fang terminal color query problem (fang issue 104) gets fixed upstream, so the CLI's workaround can be removed                     | 50%        |
| A10 | The macOS process that could not exit after holding 2000 guest connections in testing does not recur under the per-run connection caps | 60%        |

### Open questions

- **Q1. Upstream bug reports.** Verification found five `libkrun` problems worth reporting: the port map does not limit exposure, a NULL port map crashes, connected UDP replies are dropped under TSI, `\u` escapes in the config file are corrupted, and a TAP open failure hangs the VM. Filing them is a public action for the project owner to approve.
- **Q2. RuntimeClass overhead.** The `overhead.podFixed` value needs a measurement on a real node with the phase 1 bundle.

## Further Notes

- **Prior art.** `containerd/nerdbox` (v0.2.7) is a containerd shim on `libkrun` loaded through `purego`, with its own kernel, guest agent, and EROFS root. It runs one VM per container and documents no Kubernetes setup. `smol-machines/smolvm` (v1.34.3) is a full machine CLI on forked `libkrun`, with many features that `sbxr` leaves out on purpose.
- **Why `libkrun` 1.x.** The `libkrun` main branch is the 2.0 API, which is not compatible and not released. 1.19.8 (2026-11-06) is the latest stable release, gets backported fixes, and every major downstream project still uses 1.x.
- **Why phase 1 uses crun.** `crun` already runs `libkrun` VMs in Kubernetes-style containers with no new code. A correct one-VM-per-pod runtime needs a guest agent, the containerd sandbox API, and TAP networking, which is phase 2.
- **Why not TSI for the CLI.** With TSI, the port map only renames ports. Every guest listener appears on the host, on all interfaces when the guest binds `0.0.0.0`, and no setting prevents it.
- **Why a per-run root.** Sharing the cache directly would let a guest change the image for later runs and make concurrent runs collide.
- **Why the strict config writer.** The guest init matches values as keys, takes the first match, falls back to a root shell when parsing fails, and lets env entries change its own behavior. The writer contract closes each of these paths.
- **Graceful stop.** `libkrun` 1.x and 2.0 offer an orderly shutdown signal only on macOS arm64. A signal to the VM process stops the VM at once. Only the phase 2 guest agent gives workloads a graceful stop.
- **Exit code 125.** `sbxr` follows the Docker exit-code convention. The `libkrun` init also uses 125 for setup failures inside the guest. Both are `sbxr` failures from the user's view, and the stderr message says which.
- **Why not a block-device root.** A block-device root carries ownership without a user namespace, but the guest exit code then always reads 0, because `libkrun` returns the exit code only through a virtio-fs root.
- **fang workaround.** fang 2.0.1 queries the terminal background color on every help and error render, which waits 4 seconds on terminals that never answer. The CLI points the process stdout at a non-terminal while fang renders and restores it before any workload runs. A test guards both the timing and the restore. The workaround goes away when fang releases a fix.
- **Verification log (2026-11-13 to 2026-11-14).**
  - macOS: a `purego` program with cgo off booted Alpine through `libkrun` 1.19.8. Exit codes 0, 3, 126, and 127 passed through. Ownership xattrs worked. Unsigned binaries fail at VM creation. The config file carried quotes, tabs, newlines, and non-ASCII text intact. Boot took about 0.1 s and 115 to 220 MiB host memory. `go build` failed at 256 MiB guest memory and passed at 512 MiB.
  - GitHub runners: `ubuntu-latest` and `ubuntu-24.04` (x86_64) expose KVM after a udev rule. `ubuntu-24.04-arm`, `macos-15`, and `macos-26` have no usable hypervisor.
  - Linux rootless: correct guest ownership, setuid bits, and exit codes inside a user namespace. The AppArmor profile makes this work with the restriction on.
  - Per-run root: APFS clone on macOS and overlay in a user namespace on Linux kept the cache unchanged, isolated concurrent runs, and kept exit codes.
  - Network: TSI exposed unmapped guest listeners on all host interfaces. The in-process userspace stack published only listed ports on `127.0.0.1` and served DHCP and DNS on macOS and Linux. Without a guard, guest UDP to `0.0.0.0` reached a host loopback service and the host LAN address was reachable. A frame filter closed every tested path, and `--allow-lan` mode opened the LAN but never loopback.
  - Phase 2 TAP networking: the guest owned the pod address with ICMP, TCP, and UDP working, and the VM process ran without capabilities.
  - Phase 1 in kind: VMs ran with the pause container outside, logs, exit codes, probes, Services, port-forward, memory limits, and OOM behaved as listed. SIGTERM did not reach workloads. The `use-vc` mutating policy fixed Go and glibc DNS.
- **Council review.** Four independent reviewers (architecture, VMM and security, CLI experience, tests and CI) reviewed the design in three rounds. Their decisions are recorded in this document.
