# sbxr: Product Requirements

Status: draft, written on the go. Last update: 2026-11-15.

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
- `libkrun` has no runtime version query. `sbxr` validates `libkrun` by checking that every required symbol exists, including the network, virtio-fs, and root-share overlay functions, and validates `libkrunfw` by its version. `krunfw_get_version` reports only the ABI version (5), so `sbxr` reads the full version from the resolved library path: `libkrunfw.so.5.6.4` on Linux, `Cellar/libkrunfw/5.6.4/` from Homebrew. A missing symbol or an old `libkrunfw` is a preflight failure.
- Library directory: `SBXR_LIB_DIR` when set, otherwise the first of `/opt/homebrew/lib` (macOS), `/home/linuxbrew/.linuxbrew/lib`, `/usr/local/lib`, and the system library path that contains both `libkrun` and `libkrunfw`. `libkrun` loads `libkrunfw` by name, so the VM process starts with the library search path set to that directory.
- macOS signing (v1): the `sbxr` binary is signed ad hoc, without hardened runtime, with one entitlement, `com.apple.security.hypervisor`. Hardened runtime is not used, because it removes the `DYLD_*` variables that `libkrun` needs to find `libkrunfw`, and `libkrun` offers no other way to pass that path. Without hardened runtime, macOS does not enforce library validation, so no other entitlement is needed. Every build, including test binaries, is signed. An unsigned binary fails the `entitlement` check instead of failing at VM start.
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
- On Linux, `sbxr run` and `sbxr prune` run the preflight checks, then run the same command again inside a new user namespace and mount namespace with the user's subordinate id range mapped. The process inside executes itself once more after the maps are written, because only that execve grants it full capabilities in the namespace. It is the parent described above: it pulls and unpacks images, mounts the run root, and starts the VM process, which inherits the namespaces. The process outside forwards SIGINT, SIGTERM, and SIGHUP and returns the exit code. When it dies, the process inside gets SIGTERM and cleans up.
- The child's stdin carries the user's input only with `-i`. Otherwise it is empty. Without `-t`, the parent relays stdin (with `-i`), stdout, and stderr through pipes, so the VM process never sees a terminal. When a write to stdout fails because its reader went away (EPIPE), the parent stops the VM like a signal does, cleans up, and exits 141 without an `sbxr` error line. Failed writes to stderr are ignored. `libkrun` sends output to a terminal through its console, which would mix the streams and lose stderr when only stdout is redirected.
- A signal to the parent stops the VM at once (no graceful guest shutdown in the CLI). The parent then exits 130 or 143, or 129 after SIGHUP (closed terminal).
- The VM process ignores SIGINT, SIGQUIT, and SIGHUP, so terminal signals end a run only through the parent, which removes the run state. A VM process that a signal ends anyway is an `sbxr` failure: exit 125 and `sbxr: vm: the VM process was killed by signal N; run again with --debug and report a bug`.
- With `-i -t`, the parent restores the user's terminal mode as soon as the VM process exits, so that Ctrl-C can end a drain of terminal output that a slow reader holds back.
- The VM process gets SIGWINCH from the parent after each terminal resize, because its pseudo-terminal is not its controlling terminal. `libkrun` 1.19.8 applies later resizes only on Linux. On macOS the guest keeps the size from the start.
- `libkrun` aborts the process on some invalid inputs instead of returning an error. `sbxr` validates every value before it calls `libkrun`, so an abort is always an `sbxr` bug.
- In Kubernetes phase 2, the VM process is a child of the containerd shim, in a hidden mode of the shim binary. See "Kubernetes phase 2: sbxr runtime".

### Root filesystem

- Image cache: the host pulls each image and unpacks it once into the cache. Runs never write to the cache.
- Unpacking applies OCI whiteouts, keeps hardlinks and setuid bits, and skips device nodes (the guest mounts its own `/dev`).
- File ownership:
  - On macOS, image ownership and modes are stored as `user.containers.override_stat` extended attributes (`uid:gid:mode`), which `libkrun` reads.
  - On Linux, the unpack runs inside the user namespace, so files get real mapped ownership. Cache files then belong to subordinate ids, and `sbxr prune` removes them inside the user namespace.
- Per-run root:
  - On macOS, `sbxr` clones the cached image directory with an APFS clone (about 0.1 s and a few MB for a 250 MB image).
  - On Linux, `sbxr` mounts an overlay inside the user namespace: the cached image as the lower layer, a per-run upper layer, and the `userxattr` option. It uses the file system context API (`fsopen`), so cache paths may contain commas. Upper layers are never reused as lower layers.
- The guest root is the per-run root, shared over virtio-fs. The exit code comes back through virtio-fs, so the root must be virtio-fs.

### Workload config

- The workload command, env, working directory, uid, and gid reach the guest through a config file, `/.krun_config.json`, that `sbxr` writes into the per-run root. The kernel command line is not used, because it breaks on quotes and non-ASCII text and lets values override the file.
- `libkrun` call sequence: create the context, set CPUs and memory, set the root, set an empty env list, add the network device (with `--net`), add one virtio-fs device per volume and the mount helper overlay (with `-v`), disable the implicit vsock device, on macOS add a vsock device without TSI features and without port map (it carries only the host time sync to the guest init), make the virtual terminal `tty0` the kernel console (only when no stdio stream is a terminal), start.
- The `tty0` kernel console works around a `libkrun` 1.19.8 race. The default console `hvc0` appears only after a virtio handshake with the VM process. Under host load, the guest kernel starts the init before `hvc0` exists, the init starts with closed stdio, and its redirect to the `krun-stdout` port then leaves the workload without stdout (EBADF, about 1 in 4 runs with 16 parallel VMs on an M4 Pro Mac). `tty0` registers at boot on every architecture and shows nothing, so the init always has stdio. A rarer second race remains: when the guest init looks for the named ports before the handshake names them, the workload writes to the console, and its output is lost (about 1 in 80 runs at that load, none in 200 runs with 8 parallel VMs). With `-t`, the console is the workload's terminal, so it stays `hvc0` and only the first race applies. `libkrun` main fixed the closed-stdio case in its new init (upstream issue 749). `sbxr` never calls the functions that set the exec command, working directory, resource limits, or port map, and never passes a NULL env list.
- The guest init parser treats any string as a possible key and takes the first match. The writer therefore follows this contract:
  - Keys appear once each, in this order: `uid`, `gid`, `WorkingDir`, `Env`, `Entrypoint` (always empty), `args`.
  - `uid` and `gid` are non-empty decimal strings. An empty uid would run the workload as root.
  - `WorkingDir` is an absolute path.
  - Each env entry matches `NAME=value`, where NAME starts with a letter or underscore and contains only letters, digits, and underscores.
  - Env names that start with `KRUN_` and the name `HOSTNAME` are rejected from user input and dropped from the image env, because the guest init reads them as control settings.
  - `PATH` is always set, from the image or a default.
  - `args` is not empty.
  - Strings are valid UTF-8. The writer escapes only `"`, `\`, tab, and newline, and never writes `\u` escapes, because the guest init corrupts them or ignores the whole file. Other control characters, including C1 (U+0080 to U+009F), are rejected.
  - The file has at most 16384 tokens, the parser limit. Above that limit the guest init ignores the file and starts a root shell, so `sbxr` rejects such input.
  - Duplicate env names are rejected, because the guest init keeps the first value of most names and the last value of `HOME` and `TERM`.
  - A uid or gid of 4294967295 is rejected, because the guest init cannot set it and exits with 125.
  - The guest init matches keys case-insensitively and reads `Cmd` and `Cwd` as aliases for `args` and `WorkingDir`. The fixed key order keeps this safe, because no value before `args` can look like a key.
  - The file is written after removing any existing entry, with create-exclusive and no-follow semantics.
- With `-v`, the guest init cannot mount the volumes itself: the `libkrun` 1.19.8 init mounts only system file systems and drops to the workload user before it runs the workload. `sbxr` therefore runs a small static mount helper first:
  - `libkrun` serves the helper from memory at `/.sbxr-mount` in the root share (`krun_fs_add_overlay_file`), so the run root has no such file. The file is one-shot: after the init runs it, the workload does not see it.
  - The config keeps the contract below, with `uid` and `gid` set to `0` and `args` starting with the helper, then `UID:GID`, the working directory, the mount count, one `TAG:rw:PATH` or `TAG:ro:PATH` entry per volume, and the workload `args`. The tag is `sbxr-` and 16 hex digits of the SHA-256 of the guest path.
  - The helper walks each guest path inside the guest like the kernel: it follows symlinks, including symlinks in earlier volumes, fails when a component is not a directory, and creates each missing component as a directory, so `..` and later symlinks apply to real directories. It mounts on the resolved path and fails with exit 125 when that path is the guest root or reserved (`/dev`, `/proc`, `/sys`, `/.sbxr-mount`, `/.krun_config.json`, `/init.krun`). `internal/spec` checks the same list on the path as typed.
  - The helper resolves each mount point once, right before its mount. A workload cannot run yet, but anything that changes an rw volume on the host during startup can redirect a later mount, and only to a place inside the guest.
  - Volumes are ordered and deduplicated by the cleaned guest path as typed. Two volumes whose paths resolve to the same place stack, and the later one wins.
  - The helper mounts each virtio-fs tag with `nosuid` and `nodev`, plus `ro` for `:ro`. It then changes to the working directory again, drops the supplementary groups, sets the gid and uid like the init, and runs the workload with a `PATH` search like `execvp`. It keeps the init's messages and exit codes (126, 127, 125 for setup failures).
  - Runs without `-v` do not use the helper, and their config is unchanged.
- Volume access:
  - `:ro` is enforced by the host: `libkrun` exposes the device read-only, and the guest also mounts it read-only.
  - A mount point that does not exist inside an earlier volume is created in that volume, so it appears in the host directory. Under a `:ro` volume it cannot be created, and the run fails with exit 125.
  - macOS uses the `libkrun` permission semantics "Linux simplified": every guest user sees itself as the owner of the volume files and can read and write them, files that the guest creates belong to the invoking user on the host, and the guest cannot change ownership.
  - Linux uses the default semantics inside the user namespace. Guest root is the invoking user, so the workload as root reads and writes the user's files, and its new files belong to the invoking user. Other guest users map to the user's subordinate ids: they see the user's files as owned by root, get access only through the "other" permission bits, and their new files belong to subordinate ids on the host.
- Commands are not resolved on the host. Bare names reach the guest init, which searches the image `PATH`.
- Image user names in `User` are resolved by reading the image's `/etc/passwd` and `/etc/group` inside the cached image root. Path resolution never leaves that root. An unknown name fails with exit 125.
- The workload gets no supplementary groups. This differs from `docker run`.

### CLI contract

Commands:

| Command                                 | Purpose                                                                                                              |
| --------------------------------------- | -------------------------------------------------------------------------------------------------------------------- |
| `sbxr run [flags] IMAGE [CMD [ARG...]]` | Run a workload in a new VM, in the foreground, and remove it after exit                                              |
| `sbxr doctor [--json]`                  | Check the machine and print fix commands                                                                             |
| `sbxr doctor apparmor-profile`          | Print the AppArmor profile to stdout (Linux)                                                                         |
| `sbxr prune [--force]`                  | Remove cached images. Asks for confirmation on a terminal. Requires `--force` otherwise. Fails while a run is active |
| `sbxr completion SHELL`                 | Print shell completion (from fang)                                                                                   |
| `sbxr --version`                        | Print version (from fang)                                                                                            |

`sbxr` without arguments prints a short usage text with example `run` commands, including one with `--net`.

`run` flags:

| Flag                            | Default                       | Meaning                                                                                                   |
| ------------------------------- | ----------------------------- | --------------------------------------------------------------------------------------------------------- |
| `--cpus N`                      | 2, at most the host CPU count | vCPUs, 1 to host CPU count                                                                                |
| `--memory SIZE`                 | `1G`                          | Guest RAM, for example `512M`, `2G`                                                                       |
| `-e, --env KEY=VALUE`           | none                          | Set an env var. Repeatable. `KRUN_*` and `HOSTNAME` are rejected                                          |
| `--env-file PATH`               | none                          | Read env vars from a file. Same rules as `-e`                                                             |
| `-v, --volume HOST:GUEST[:ro]`  | none                          | Share a host directory at an absolute guest path. `:ro` makes it read-only. Repeatable                    |
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
| 129           | Stopped by SIGHUP, for example a closed terminal            |
| 130           | Interrupted by SIGINT                                       |
| 141           | The reader of stdout went away, like SIGPIPE in a pipeline  |
| 143           | Stopped by SIGTERM                                          |

A workload that exits 125, 126, or 127 cannot be told apart from an `sbxr` failure by exit code. `sbxr` failures always print a message that starts with `sbxr:` on stderr. Other commands exit 0 on success, 1 on failure, and 2 on usage errors.

Output streams:

- stdout carries only workload output (`run`) or the command result (`doctor --json`, `doctor apparmor-profile`).
- stderr carries logs, progress, warnings, and errors.
- Progress shows only when stderr is a terminal and `-q` is not set: one line, `Pulling IMAGE: layer N/M, X MB`, redrawn in place and cleared when the pull ends. It uses no escape codes. `run` prints nothing of its own on success unless `--debug` is set. With `--debug`, each published port is logged, for example `published 127.0.0.1:8080 -> 80/tcp`.
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
| `libkrunfw`   | all      | The library loads, reports ABI version 5, and its resolved path shows version 5.6.4 or later                     |
| `entitlement` | macOS    | The running binary is signed with the hypervisor entitlement                                                     |
| `userns`      | Linux    | Inside a new user namespace with the user's subordinate ids, a chown to uid 0 gid 42 and a private mount succeed |
| `cache`       | all      | The cache directory is writable                                                                                  |

When the `userns` check fails and the AppArmor restriction is on, `sbxr doctor` checks the running binary's resolved path. For `/usr/bin/sbxr` or `/usr/local/bin/sbxr` it prints the profile install commands, for example `sbxr doctor apparmor-profile | sudo tee /etc/apparmor.d/sbxr && sudo apparmor_parser -r /etc/apparmor.d/sbxr`. For any other path it tells the user to install to `/usr/local/bin`.

On Linux, a text report with a failed check ends with the setup block from "Distribution".

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

`status` is `pass` or `fail`. `fix` is present only when `status` is `fail`. The JSON schema is [doctor.schema.json](doctor.schema.json).

### Networking (CLI)

- Default: no network. The VM has no network device, and its vsock device (macOS only) has no TSI features and no port map, so socket forwarding (TSI) is off and the guest can neither connect out nor listen on the host. A real-VM test checks TCP in both directions and a vsock connection to the host.
- `--net`: the guest gets a virtio-net device connected to a userspace network stack inside the `sbxr` parent process. The guest configures itself with DHCP and uses the stack's gateway for DNS. No helper binary and no socket file are involved.
- `libkrun` socket forwarding (TSI) is never used for CLI networking, because it exposes every guest listener on all host interfaces and cannot be limited.
- Egress policy, enforced by `sbxr` for every new TCP connection and UDP flow:
  - Always blocked: `0.0.0.0/8`, `127.0.0.0/8`, `169.254.0.0/16` (including cloud metadata), `224.0.0.0/4`, `240.0.0.0/4`, the gateway address except DNS and DHCP, and all non-IPv4 traffic except ARP.
  - Blocked unless `--allow-lan`: `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10`, and every address of the host's own interfaces (re-read while the run is active).
  - Also always blocked: the rest of the guest subnet, `255.255.255.255` except DHCP, DHCP to any other address, and guest TCP to the gateway except DNS and replies from published ports.
  - Frames whose source MAC address is not the guest MAC, and ARP from any other sender address, are dropped, so the switch learns no other MAC address.
  - Allowed: all other IPv4 destinations.
- Enforcement has two layers, because the network library dials any destination it is given:
  - A frame filter between the VM and the network stack drops blocked frames before the stack sees them. Without it, guest UDP to `0.0.0.0` reached a host loopback service in testing.
  - A blocked TCP connection gets a reset from the frame filter, so it fails at once. Blocked UDP and other traffic is dropped silently.
  - `sbxr` builds the network stack from the library's packages and installs its own TCP and UDP forwarders, which check the destination again at dial time and count flows.
- `-p`: one forward per mapping, created by the network stack on the host. The default bind address is `127.0.0.1`. Only listed ports are reachable.
- Guest addresses are fixed: subnet `192.168.127.0/24`, gateway `.1`, guest `.2`. A host LAN in the same range is not reachable from the guest, even with `--allow-lan`.
- Resource limits: each run allows at most 1024 established TCP connections and 256 UDP flows. Past the cap, new TCP connections get a reset and new UDP flows are dropped. The library alone limits only half-open TCP connections, and in testing a guest held 2000 connections, each costing one host file descriptor.
- Published connections and DNS TCP connections count against the TCP cap. A published or DNS TCP connection past the cap gets a reset.
- Gateway DNS: at most 64 queries resolve at once, and further queries are dropped. Each query has a 2-second timeout. A queries go to the host resolver, AAAA queries get an empty answer because the guest has no IPv6 route, and other types go to the nameservers in `/etc/resolv.conf`. Forwarded queries are rebuilt with one `IN` question, recursion desired, and no guest options. A truncated UDP answer is asked again over TCP. The gateway refuses non-`IN` classes and does not implement other opcodes or zone transfers.
- The core defines a network backend interface. The userspace stack is one adapter used by the CLI. The phase 2 TAP network is another adapter. The core and the fake test VM do not depend on the userspace stack.

### Run lifecycle

States for one run: `validating` -> `pulling` -> `preparing` -> `running` -> `exited` -> `cleaned`.

- Any failure before `running` goes to `cleaned` with exit 125.
- A signal in any state goes to `cleaned` with exit 130, 143, or 129.
- `cleaned` is reached on every path: the VM process is gone, the network stack and its listeners are closed, and the per-run root is removed. Run state never outlives the `sbxr run` process.
- Concurrent runs share the image cache read-only. Writes to the cache (pull, unpack, prune) are locked. Each run has its own root and state.
- Each run holds a shared lock on the cache from before the pull until its root is removed. `sbxr prune` takes the lock exclusively without waiting, so it fails with a message while any run is active.
- When the parent dies without cleanup (for example by SIGKILL), the VM process sees its lifeline pipe close, unmounts the run root, removes the run directory, and exits.
- Each run holds an exclusive lock on a lock file next to its run directory from before the directory exists. The VM process inherits this lock and the shared cache lock, so a VM that outlives its parent keeps its run root and the cache. When the parent and the VM process both die, `sbxr run` and `sbxr prune` remove the run directories whose lock is free.

### Kubernetes contract, both phases

- RuntimeClass name and handler: `sbxr`.
- RuntimeClass `scheduling.nodeSelector`: `sbxr.dev/ready: "true"`.
- RuntimeClass `overhead.podFixed`: memory for the guest kernel and VMM, 64 MiB. In kind, the container cgroup of an idle phase 1 pod held 20 MiB more than the guest used for a 256 MiB guest, and 45 MiB more for a 2 GiB guest: about 15 MiB plus 15 MiB per GiB of guest memory. 64 MiB covers guests up to about 3 GiB.
- RuntimeClass annotations: `sbxr.dev/phase` (`"1"` or `"2"`), `sbxr.dev/limits` (comma-separated list of known limits), `sbxr.dev/docs` (URL).
- Installer: one DaemonSet manifest, release asset `sbxr-install.yaml`, with the installer image `ghcr.io/albertsko/sbxr-node` pinned by digest and artifact checksums verified.
- Supported first: Kubernetes 1.36 or later, where `MutatingAdmissionPolicy` v1 is served by default. Node distributions: kubeadm-style nodes with containerd 2.2 or later and the default `conf.d` import, k3s and RKE2 with the `config-v3.toml.d` drop-in directory, and k0s 1.36 or later with `/etc/k0s/containerd.d`. kind is supported through a documented cluster config patch, set at cluster creation. Managed cloud node pools come later.
- Installer steps on each node:
  1. Detect the distribution from its systemd units.
  2. Run preflight: `/dev/kvm`, glibc 2.35 or later, containerd version, main config version 2 or 3, an `imports` entry that covers the drop-in directory, and the D-Bus system socket when runc uses systemd cgroups.
  3. If preflight fails, change nothing, leave the node unlabeled, and write the reason to the node annotation `sbxr.dev/preflight`. The installer never edits a main containerd config or template.
  4. Install binaries and libraries under one directory, then write the `sbxr` drop-in file atomically under a host lock, and restart the containerd unit only when the content changed.
  5. Verify that the unit is active and that a node status posted after the restart reports handler `sbxr`. Then set the ready label. If the restart or verification fails, restore the previous drop-in (or remove it) and restart again. If that rollback fails, the installer exits with an error and retries.
- Uninstall, release asset `sbxr-uninstall.yaml`: `kubectl apply` replaces the installer DaemonSet with one that removes the label and annotation, removes the drop-in of every supported distribution, restarts containerd, and removes the binaries. It keeps the binaries while containerd has not restarted or `sbxr` containers still have state under the handler's runtime root (`/run/sbxr/runc`), and reports why in the annotation. `kubectl delete -f` of the same file then removes the RuntimeClass, the policies, and the namespace.
- containerd pulls the container image, not `sbxr`.
- Admission policies shipped with the installer, for pods with `runtimeClassName: sbxr`:
  - Validating, deny: privileged containers, host network, host PID, host IPC, added capabilities.
  - Validating, warn: exec probes and lifecycle exec hooks (phase 1), `terminationGracePeriodSeconds` over 10 (phase 1).
  - Mutating (phase 1): add the DNS option `use-vc` to the pod DNS config, keeping existing options and not adding it twice.

### Kubernetes phase 1: crun with libkrun

- The installer ships `crun` built with `libkrun` support, plus `libkrun`, `libkrunfw`, and the libraries they need that nodes may lack (for example `libjson-c`). No distribution package provides this build.
- The bundle builds on Ubuntu 22.04 (glibc 2.35) and installs to `/opt/sbxr` (33 MiB; the phase 2 files add about 27 MiB). `crun` has the absolute RUNPATH `/opt/sbxr/lib`, because it re-executes itself from a memfd, where `$ORIGIN` resolves to `/`. The libraries use `$ORIGIN`.
- The bundle patches the `libkrun` guest init so that the last of duplicate env entries wins. containerd puts the pod env after the image env, so a pod env value overrides the image value, as with `runc` (Q1, item 6).
- containerd drop-in for handler `sbxr`: runtime type `io.containerd.runc.v2`, with the binary set to a `krun`-named link to that `crun` and the runtime root set to `/run/sbxr/runc`, so that the installer can tell whether any `sbxr` container exists. The installer passes no `krun.*` annotations to `crun`, because they enable nested virtualization, custom kernels, and TAP devices.
- The drop-in uses the main config's version (kind writes 2, k3s, RKE2, and k0s write 3), because containerd rejects a version 3 drop-in under a version 2 main config. Its `SystemdCgroup` matches the node's runc runtime. With systemd cgroups, `crun` needs D-Bus, unlike `runc`. The kind node image has no D-Bus, so the kind setup installs it.
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

- containerd runtime type `io.containerd.sbxr.v1`, shim binary `containerd-shim-sbxr-v1`. containerd uses the shim as the pod sandboxer (containerd sandbox API, `sandboxer = "shim"`), so the pod has no pause container.
- containerd's shim sandboxer finds the shim only by its name, in `PATH` or next to the containerd binary, and ignores `runtime_path`. The installer links `/usr/local/bin/containerd-shim-sbxr-v1` to the bundle, and uninstall removes the link.
- One VM per pod. The VM process is a child of the shim, in a hidden mode of the shim binary, because the shim keeps the privileges that mounting container roots needs. The VM process:
  - Runs as root of its own user namespace, which maps 65536 ids to a slot of the `sbxr` range (claims under `/run/sbxr/ids`). It has no capabilities on the host.
  - Stays in the host mount namespace. A private mount namespace with only its files would need a pivot root per pod, so it waits until a threat needs it.
  - Is not dumpable, sets no-new-privileges, and dies with the shim (parent death signal).
  - Starts in the pod network namespace and directly in a child cgroup of the pod cgroup (`CLONE_INTO_CGROUP`), so the pod memory limit plus the RuntimeClass overhead covers it from the start. A pod without a cgroup parent fails to start.
  - Opens the TAP device itself before it starts the VM, and fails if that fails.
- The guest agent runs as PID 1: the `libkrun` built-in init executes it as PID 1 (`KRUN_INIT_PID1=1` in the guest root's `.krun_config.json`). The guest root is `/opt/sbxr/guest`: the agent and a static `crun` (release build without systemd), shared read-only with every pod VM.
- Guest agent responsibilities:
  - Mount system filesystems and the pod share, reap processes, set hostname and pod networking.
  - Create and start the pod's containers with `crun` inside the guest. The containers share the guest's network, IPC, and UTS namespaces. Each has its own PID, mount, and cgroup namespaces.
  - Run exec processes and resize terminals.
  - Carry stdin, stdout, and stderr as separate streams.
  - Forward signals, report exit codes and start errors, and power off the guest when the sandbox stops.
- Graceful stop: the kubelet stops each container through the task API (SIGTERM, grace period, SIGKILL in the guest). Then the shim asks the agent to power off, and kills the VM process if the guest is still running after the stop timeout.
- Host ids: the installer reserves `sbxr:<first>:33554432` (512 pods of 65536 ids) in `/etc/subuid` and `/etc/subgid`, apart from every other entry. Without a `kubelet` entry, the kubelet gives pods with `hostUsers: false` random ids from almost the whole id space, so preflight requires the `kubelet` user, `getsubids`, and an equal `kubelet` entry in both files. Uninstall removes the `sbxr` entries. The `kubelet` range comes from `getsubids`, as the kubelet reads it, and must follow the kubelet's rules: one equal uid and gid range, a first id that is a multiple of 65536 and at least 65536, and a length that holds the node's pod capacity in 65536-id steps. Preflight cannot detect a kubelet that was not restarted after the range changed. The installer edits the files under shadow's `.lock` files and keeps their mode and owner.
- Pod directory `/run/sbxr/pods/<id>`: mode 0750, owned by root with the pod's group, so other pods cannot read it. Everything the shim creates in it, including the share, belongs to root and is at most readable by the pod's group, so neither the guest nor the VM process can create, rename, or replace an entry the shim uses. Only the VM's socket directory and the mounted container roots and volumes are writable by the pod. The shim resolves paths below the pod directory with `openat2` (no symlinks, no mount crossings), mounts on `O_PATH` descriptors, connects to the VM's sockets only after it checks their type and owner, and removes nothing while a mount remains.
- Container roots and volumes: the shim mounts them into the pod share (a virtio-fs share that the guest mounts at `/run/sbxr/shared`) and rewrites the container spec for the guest. Overlayfs cannot be idmapped, so the shim idmaps each lower layer into the pod's id range and mounts the overlay over them. The upper layer stays on the host and belongs to the pod's range. Volumes are idmapped binds, with a plain bind on file systems without idmapped mounts. This needs Linux 5.19 or later.
- The shim writes `/etc/resolv.conf` and `/etc/hostname` for the containers, because containerd writes them only for its own sandbox controller.
- VM memory is the sum of the container memory limits, or 1024 MiB without one. VM vCPUs are the pod CPU limit rounded up, or every node CPU without one.
- Host-guest protocol: TTRPC with the containerd task API on vsock port 1024, plus vsock port 1025 for raw streams. Each stream starts with one header line that names the process and stream, or a port-forward target port.
- Pod networking: a TAP device in the pod network namespace, created owned by the VM process's uid, with traffic redirected both ways between the pod interface and the TAP device (tc mirred). The pod interface's addresses and routes move to the guest, and the guest uses the pod interface's MAC address. No socket forwarding (TSI).
- `kubectl port-forward`: containerd dials `localhost` in the pod network namespace, where nothing listens. An `iptables` rule there redirects local TCP connections to a shim listener, which opens a port-forward stream to the agent. The node needs `iptables` and `nsenter`; without them, everything else works.
- A failed sandbox start stops the VM and undoes the TAP device, the redirect, the moved addresses, and the port-forward rule. containerd does not retry a start: the kubelet creates a new sandbox. A failed host stream setup after the guest created a process removes the process from the guest again.
- Fail fast: a boot watchdog stops the VM when the guest agent does not answer within 30 seconds, and the sandbox start fails with the VM log tail.
- Phase 2 needs neither phase 1 admission policy (`sbxr-warn`, `sbxr-dns-use-vc`): exec probes, SIGTERM, and DNS over UDP work. Its preflight adds Linux 5.19, `/dev/net/tun`, and the `kubelet` id range, and drops the D-Bus check.
- These phase 1 criteria change in phase 2: exec probes and long grace periods get no admission warning, and each pod runs one VM with no pause container.
- Known limits, shown in the RuntimeClass annotations and the docs:
  - No per-container metrics (`kubectl top` shows the pod's cgroup).
  - `shareProcessNamespace` is ignored. Each container has its own PID namespace.
  - No `kubectl attach`.
  - Moving from phase 1 needs one manual step: delete the two phase 1 policies, because `kubectl apply` does not remove objects.

### Test contract

- Behavior scenarios are written once and run against several targets: `fake` (no hypervisor), `cli` (real VM), `k8s-p1` (kind cluster with phase 1), and `k8s-p2` (kind cluster with phase 2, upgraded from phase 1).
- Each target declares its capabilities. Each scenario declares the capabilities it needs and runs only on targets that have them. Scenarios never check the target name.
- Capabilities: `stdin`, `tty`, `split-stderr`, `signal-exit`, `graceful-stop`, `exec`, `net`, `net-off`, `publish`, `dns`, `volumes`, `memory-limit`, `probes`, `spec-validation`, `exact-config`, `egress-guard`.
- `spec-validation`: the target rejects invalid run input (relative workdir, control characters, reserved env names) before anything starts. `exact-config`: the workload sees the image config and the run flags and nothing else. Kubernetes targets lack both, because Kubernetes accepts such input, adds service env and `HOSTNAME`, and does not apply the group of a numeric image `USER`.
- `egress-guard`: `--net` and `--allow-lan` select the egress policy of "Networking (CLI)", and the host resolver answers DNS. Kubernetes targets lack it, because pods use the pod network and cluster DNS.

  | Target   | Capabilities                                                                                                                                    |
  | -------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
  | `fake`   | `stdin`, `tty`, `split-stderr`, `signal-exit`, `net`, `net-off`, `publish`, `dns`, `volumes`, `spec-validation`, `exact-config`, `egress-guard` |
  | `cli`    | same as `fake`                                                                                                                                  |
  | `k8s-p1` | `net`, `publish`, `dns`, `memory-limit`, `probes`                                                                                               |
  | `k8s-p2` | `k8s-p1` plus `exec`, `graceful-stop`                                                                                                           |

- Scenarios assert on behavior: stdout (or combined output), exit code, and a target-neutral error class. Error classes: `ok`, `invalid-spec`, `image`, `preflight`, `not-found`, `not-executable`, `interrupted`. Each target maps its own signals to these classes. Scenarios never assert the order across stdout and stderr.
- After each scenario, the target verifies that no VM process, network listener, pod, or run state remains.
- The fake target runs the real userspace network stack against a small simulated guest network, so port publishing, egress blocking, and cleanup are tested without a hypervisor.
- Network scenarios need no public address. They serve HTTP on a non-loopback address of the host, which the guest reaches only with `--allow-lan`, and publish a guest port on a free port of `127.0.0.1`. The netstack unit tests cover public destinations through a dial hook.
- The config file writer is tested with property and fuzz tests against a copy of the guest init parser. The seed corpus lives in `internal/krunconfig/testdata/fuzz/FuzzMarshal`. The hypervisor job also boots every seed that the writer accepts through the real guest init and compares the arguments, env, uid, gid, and working directory that the workload sees.
- Hypervisor detection uses the same rules as the `hypervisor` doctor check.
- `SBXR_REQUIRE_HV=1` turns hypervisor skips into failures. CI sets it on hypervisor jobs.
- The `fake` and `cli` targets pull the scenario images from a registry inside the test process on `127.0.0.1`, so both exercise pull, cache, and run root. The `fake` target builds them on a fake busybox image. The `cli` target builds them on `mirror.gcr.io/library/busybox`, which serves Docker Hub library images without its rate limit, and is the only external network use of the tests.
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
| kind-p2        | `ubuntu-latest`, KVM enabled                             | phase 1 install, upgrade to phase 2, `k8s-p2` target and the phase 2 criteria                          | 10 min |
| fuzz (nightly) | `ubuntu-latest`                                          | long fuzz runs                                                                                         | 20 min |
| install-linux  | `ubuntu-latest`                                          | README setup block on a goreleaser snapshot, `doctor` passes after a new login                         | 6 min  |
| install-macos  | `macos-15`                                               | tap formula on a goreleaser snapshot, `doctor` fails only `hypervisor`                                 | 6 min  |

- KVM on `ubuntu-latest` needs a udev rule that opens `/dev/kvm` to all users. kind resets `/dev/kvm` permissions when it creates a cluster, so the kind job sets them again after creation.
- The hv job installs `sbxr` to `/usr/local/bin` and the AppArmor profile, because the runner has the user namespace restriction on.
- CI builds `libkrun` from source at the pinned version with network support, uses the `libkrunfw` release tarball, and caches both by version. The kind job builds the installer image, including the node bundle, and caches its layers by version.
- Real-VM tests on macOS and Linux arm64 run locally or on a self-hosted runner, because hosted macOS and arm64 runners have no usable hypervisor.

### Distribution

- macOS: a hand-written Homebrew formula in the tap `albertsko/homebrew-tap`. It depends on `libkrun`, installs the prebuilt release binary, and signs it with the hypervisor entitlement during install. The formula has no bottles, so Homebrew never re-signs the binary.
- Linux: `libkrun` from Homebrew on Linux, and the `sbxr` release binary installed to `/usr/local/bin`. The README and `sbxr doctor` print one setup block: install `uidmap`, add the user to the `kvm` group, install the binary, install the AppArmor profile.
- `libkrun` and `libkrunfw` come from `homebrew/core` (since 2026-11-05) on macOS and on Linux. The Linux bottle is built with `NET=1`.
- `brew install albertsko/tap/sbxr` taps the tap and trusts only the `sbxr` formula, so tap trust adds no step. This verifies assumption A5 (Homebrew 7.0.13, run [43722622849](https://github.com/albertsko/sbxr/actions/runs/43722622849)).
- Release archives and checksums come from goreleaser: `sbxr_darwin_arm64`, `sbxr_linux_amd64`, and `sbxr_linux_arm64`. The names carry no version, so `releases/latest/download/` URLs stay stable. The archives include `entitlements.plist`. No goreleaser-generated Homebrew casks.
- After a release, the tap formula gets the new version and checksum by hand. The tap README has the commands.
- `go install` works for development. `sbxr doctor` prints the signing command (macOS) and the install path rule (Linux).
- Developer ID signing and notarization come later, when `sbxr` ships outside Homebrew on macOS. Notarization requires hardened runtime, so it needs one of these: the entitlements `cs.disable-library-validation` and `cs.allow-dyld-environment-variables` (tested: the VM boots), `libkrun` and `libkrunfw` signed with the project's own Team ID plus the `DYLD_*` entitlement, or a bundled guest kernel passed to `libkrun` so that `libkrunfw` is not loaded.

### Repository and tooling

- Project base: `albertsko/go-kickstart` (mise tools and tasks, strict golangci-lint, goreleaser).
- `vendor/` is gitignored. `mise run deps` runs `go mod vendor`. CI runs the same task, so local and CI builds use the same vendored sources.
- Tasks: `deps`, `test`, `test:hv`, `test:kind`, `lint`, `build`, `ci`. The same tasks run locally and in CI.
- `go generate ./internal/guestmount` builds the mount helper for `linux/amd64` and `linux/arm64`, and `sbxr` embeds the one for its own architecture. `mise run build`, the goreleaser before-hook, and the hv test build run it. Git ignores the generated files. A build without them works, except that `-v` fails with exit 125 and asks for `mise run build`.
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

| ID  | Assumption                                                                                                                                                                                                                                                                                                                                                                                 | Confidence |
| --- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ---------- |
| A1  | `libkrun` 1.19.x stays maintained for the next 6 to 12 months                                                                                                                                                                                                                                                                                                                              | 85%        |
| A2  | One phase 1 node bundle, built on Ubuntu 22.04 with its own libraries, runs on nodes with glibc 2.35 or later. Verified on Ubuntu 22.04 and 24.04, Debian 12, Rocky 9, Amazon Linux 2023, and the kind node. Debian 11 (glibc 2.31) fails                                                                                                                                                  | verified   |
| A3  | One scenario set with capability tags covers all targets without target-specific branches inside scenarios                                                                                                                                                                                                                                                                                 | 75%        |
| A4  | RKE2 imports the `config-v3.toml.d` drop-in directory like k3s. Verified on RKE2 1.34.13 and 1.37.2, with the k3s and k0s paths. Restarting the k3s or RKE2 unit restarts containerd                                                                                                                                                                                                       | verified   |
| A5  | Homebrew 6 tap trust adds at most one documented step to the macOS install. Verified: `brew install albertsko/tap/sbxr` needs no extra step on Homebrew 7.0.13                                                                                                                                                                                                                             | verified   |
| A6  | Overlay edge cases not yet tested (hardlinks across copy-up, renaming lower directories) work over virtio-fs                                                                                                                                                                                                                                                                               | 75%        |
| A7  | The phase 2 guest agent design (containerd task API over vsock, `crun` in the guest) works with `libkrun` 1.x and its own kernel boot. Verified with `libkrun` 1.19.8 and `crun` 1.31 in kind; overlayfs needs idmapped lower layers instead of an idmapped overlay                                                                                                                        | verified   |
| A8  | MutatingAdmissionPolicy at `admissionregistration.k8s.io/v1` is available on every Kubernetes version the installer supports. Failed as first written: Kubernetes serves it by default only from 1.36, so the installer requires 1.36                                                                                                                                                      | verified   |
| A9  | The fang terminal color query problem (fang issue 104) gets fixed upstream, so the CLI's workaround can be removed                                                                                                                                                                                                                                                                         | 50%        |
| A10 | The macOS process that could not exit after holding 2000 guest connections in testing does not recur under the per-run connection caps. Verified on an M4 Pro Mac with `libkrun` 1.19.8 (see HANDOFF "Evidence"): the parent exited within 0.1 s in every run at the caps (1024 TCP, 256 UDP), above them, and with 2000 TCP connections. `TestCloseAtCaps` checks the network stack in CI | verified   |

### Open questions

These questions stay open at handoff. Each one names who decides or what work answers it.

- **Q1. Upstream bug reports.** Verification found six `libkrun` problems worth reporting to `libkrun/libkrun`. Filing them is a public action, so the project owner decides whether and when to file. No issue has been filed yet. All six were seen with `libkrun` 1.19.8 and `libkrunfw` 5.6.4. `sbxr` already works around each one, so none of them blocks implementation.
  1. **The port map does not limit exposure.**
     - Seen: under TSI, a guest listener on a port that is not in the port map still appears on the host. A guest bound to `0.0.0.0` appears on all host interfaces.
     - Contradicts: the header says an empty map exposes nothing.
     - Reproduce: map `18080:8080`, start guest listeners on 8080 and 9090, then list host listeners. 9090 shows up.
     - Seen on: macOS and Linux.
  2. **A NULL port map crashes the process** (SIGSEGV), although the header documents NULL as valid.
  3. **Replies on connected UDP sockets are dropped under TSI.**
     - Cause: the host sends replies with source port 0, and the guest kernel's source check drops them.
     - Effect: DNS fails for busybox, the Go resolver, and glibc.
     - Seen on: macOS, and in Kubernetes through `crun`.
     - Related open issues: `libkrun/libkrunfw#132` and `libkrun/libkrun#934`.
  4. **`\u` escapes in the guest config file are corrupted silently.** A `\u00e9` escape empties that value and every env entry after it, and the run still exits 0.
  5. **A TAP open failure hangs the VM.**
     - Seen: `krun_add_net_tap` returns success for a TAP device the process cannot open. Activation then fails with a log line and a vCPU panic, and the process never exits.
     - Seen on: Linux, with a build that has network support.
  6. **The guest init keeps the first of duplicate env entries.**
     - Seen: containerd lists the image env first and the pod env after it, and the init sets each entry without overwriting. A pod env value that the image also sets is lost.
     - Seen on: Kubernetes phase 1 through `crun`. The phase 1 bundle patches the init to let the last entry win.

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
- **Verification log (2026-11-13 to 2026-11-15).**
  - macOS: a `purego` program with cgo off booted Alpine through `libkrun` 1.19.8. Exit codes 0, 3, 126, and 127 passed through. Ownership xattrs worked. Unsigned binaries fail at VM creation. With hardened runtime, `libkrun` failed to load `libkrunfw` unless the binary also had `cs.allow-dyld-environment-variables`. Without hardened runtime, the hypervisor entitlement alone was enough. The config file carried quotes, tabs, newlines, and non-ASCII text intact. Boot took about 0.1 s and 115 to 220 MiB host memory. `go build` failed at 256 MiB guest memory and passed at 512 MiB.
  - GitHub runners: `ubuntu-latest` and `ubuntu-24.04` (x86_64) expose KVM after a udev rule. `ubuntu-24.04-arm`, `macos-15`, and `macos-26` have no usable hypervisor.
  - Linux rootless: correct guest ownership, setuid bits, and exit codes inside a user namespace. The AppArmor profile makes this work with the restriction on.
  - Per-run root: APFS clone on macOS and overlay in a user namespace on Linux kept the cache unchanged, isolated concurrent runs, and kept exit codes.
  - Network: TSI exposed unmapped guest listeners on all host interfaces. The in-process userspace stack published only listed ports on `127.0.0.1` and served DHCP and DNS on macOS and Linux. Without a guard, guest UDP to `0.0.0.0` reached a host loopback service and the host LAN address was reachable. A frame filter closed every tested path, and `--allow-lan` mode opened the LAN but never loopback.
  - Phase 2 TAP networking: the guest owned the pod address with ICMP, TCP, and UDP working, and the VM process ran without capabilities.
  - Phase 1 in kind: VMs ran with the pause container outside, logs, exit codes, probes, Services, port-forward, memory limits, and OOM behaved as listed. SIGTERM did not reach workloads. The `use-vc` mutating policy fixed Go and glibc DNS.
  - Phase 1 installer in kind (Kubernetes 1.37, containerd 2.3): the installer passed preflight, installed the bundle, and labeled the node. The phase 1 acceptance criteria and the `k8s-p1` scenarios passed, and uninstall left nothing behind. A node without the drop-in import stayed unchanged and unlabeled, with the reason in its annotation.
  - Phase 2 (2026-11-15): the `libkrun` init started the agent as PID 1, vsock ports with `listen` connected host sockets to guest listeners, a VM process in its own user namespace opened a TAP device created for its uid, tc redirect carried ICMP both ways, static `crun` ran containers in the guest, and mounts made in a virtio-fs share after boot appeared in the guest. An idmapped bind of an overlay failed (EINVAL), while an overlay over idmapped lower layers worked. In kind, the phase 2 criteria and the phase 1 criteria passed after the node upgrade.
  - Fresh install (2026-11-15): on `ubuntu-latest`, the README setup block with Homebrew `libkrun` 1.19.8 and a new login made every doctor check pass. The runner's `/dev/kvm` needed only the `kvm` group. On `macos-15` with Homebrew 7.0.13, `brew install albertsko/tap/sbxr` tapped the tap, trusted the formula, installed `libkrun`, and signed `sbxr`. Doctor failed only `hypervisor`.
- **Council review.** Four independent reviewers (architecture, VMM and security, CLI experience, tests and CI) reviewed the design in three rounds. Their decisions are recorded in this document.
