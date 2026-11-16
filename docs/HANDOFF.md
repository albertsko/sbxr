# sbxr: Handoff

Status: steps S1 to S8 are done, and [v0.1.0](https://github.com/albertsko/sbxr/releases/tag/v0.1.0) is released. The PRD stays a living document. Last update: 2026-11-16.

**Next: no build step is open.** See "Open work" for what remains.

The design decisions in [PRD.md](PRD.md) are final unless a failed assumption or an open question reopens them (see "Changing a decision"). This handoff tells the next engineer or AI agent how to continue. It does not repeat the PRD. When the two disagree, the PRD wins, so fix this handoff.

## Read first

1. [PRD.md](PRD.md), sections "Solution", "Implementation Decisions", and "Assumptions and Open Questions".
2. [AGENTS.md](../AGENTS.md) for repository rules (tasks, commits, releases).
3. "Gotchas" below, before you write the first line of VM code.

## Terms

This handoff uses these terms. The PRD uses the same ones, except "egress guard", which implements PRD "Egress policy".

- **Run:** one `sbxr run` invocation, from validation to cleanup.
- **Workload:** the program from the image that runs inside the VM.
- **VM process:** the process that calls `libkrun` and becomes the VM. The `sbxr run` process starts it as a child.
- **Target:** a backend that the behavior scenarios run against (`fake`, `cli`, `k8s-p1`, `k8s-p2`). See PRD "Test contract".
- **Userspace network stack:** `gvisor-tap-vsock`, used as a library inside the `sbxr run` process.
- **Egress guard:** the frame filter plus the dial-time check that enforce the PRD egress policy.
- **Config writer:** the code that writes `/.krun_config.json` by the PRD "Workload config" contract.

## Current state

- `sbxr run` boots real VMs with `libkrun` on macOS arm64 and on Linux with KVM, without root. `sbxr doctor` runs every check of the PRD contract, and `sbxr run` runs them first. `sbxr prune` works and fails while a run is active.
- Images come from registries through `internal/image`: pull policy, a cache with a shared run lock, `User` name lookup, and a run root per run (APFS clone on macOS, overlay in the user namespace on Linux).
- On Linux, `sbxr run` and `sbxr prune` run again inside a user namespace (`internal/userns`) after the checks. See PRD "Process model".
- The fake build (`-tags sbxrfake`) replaces only the hypervisor and the doctor checks. It pulls images like the release build and keeps ownership in xattrs on every platform, so it needs no user namespace.
- `--net`, `--allow-lan`, and `-p` work. The `sbxr run` process hosts the userspace network stack (`internal/netstack`) and passes the VM end of a datagram socket pair to the VM process, which adds it with `krun_add_net_unixgram` in fd mode without `NET_FLAG_VFKIT`. The A10 spike passed: at the caps (1024 TCP, 256 UDP), the stack closes in milliseconds on macOS.
- The fake VM runs `internal/netstack/guestsim`, a small gvisor guest stack with DHCP, so the network scenarios run without a hypervisor. Its busybox has `wget`, `nc`, and `nslookup`.
- The connection caps have no scenario, because the fake busybox has no loops or background jobs. The netstack tests cover them: `TestTCPCap`, `TestUDPCap`, and `TestCloseAtCaps` (guest connections past the cap fail, and the stack keeps working and closes at once).
- `-v` shares host directories as virtio-fs devices. A small static helper (`cmd/sbxr-mount`, embedded through `internal/guestmount` by `go generate`) mounts them in the guest before the workload, because the `libkrun` init cannot. See PRD "Workload config". Build with `mise run build`, or run `go generate ./internal/guestmount` first, because a bare `go build` embeds whatever helper is on disk.
- Distribution (S6): goreleaser builds the release archives on `v*` tags. The formula `albertsko/homebrew-tap/Formula/sbxr.rb` installs `sbxr_darwin_arm64` and signs it, and points at the latest release. After each release, update its URL and checksum by hand (tap README). The Linux setup block lives in `internal/preflight/setup.go` and the README, and a test keeps them equal. The `install` workflow installs a goreleaser snapshot on fresh `ubuntu-latest` and `macos-15` runners (PRD "CI contract").
- Packages:
  - `internal/spec`: run input validation, no I/O.
  - `internal/krunconfig`: the config writer.
  - `internal/run`: the run lifecycle, the VM process, stdio, terminal, signals, and cleanup.
  - `internal/vm`: the VM process side, with the parent lifeline. `internal/vm/libkrun` is the `libkrun` binding. `internal/vm/fake` is the fake hypervisor, its busybox image, and its doctor checks.
  - `internal/guestmount` and `cmd/sbxr-mount`: the guest mount helper for `-v` and its command line.
  - `internal/netstack`: the userspace network stack, the egress guard, the caps, the gateway DNS, and `-p`. `guestsim` is the simulated guest for tests and the fake VM.
  - `internal/hv`: hypervisor detection and the macOS entitlement check, shared by doctor and tests.
  - `internal/image`: pull, unpack, cache, run root, and the store that the runner uses.
  - `internal/userns`: the Linux user namespace with `newuidmap` and `newgidmap`, and the `userns` check.
  - `internal/preflight`: doctor checks and the `doctor --json` report. The schema is [doctor.schema.json](doctor.schema.json).
  - `internal/cli`: cobra, fang, and output. The only package that imports them.
  - `internal/scenario`: behavior scenarios, the scenario images and their registry, the `fake` and `cli` targets, and the `k8s-p1` and `k8s-p2` targets (build tag `kind`).
  - `internal/node` and `cmd/sbxr-node`: the node installer for phase 1 and phase 2 (`SBXR_PHASE`, default 2). `deploy/node` builds its image, with the node bundle, and holds the manifest template. [kubernetes.md](kubernetes.md) is the user guide.
  - `internal/shim` and `cmd/containerd-shim-sbxr-v1`: the phase 2 containerd shim (sandbox and task APIs) and, in its hidden `vm` mode, the VM process.
  - `internal/agent` and `cmd/sbxr-agent`: the phase 2 guest agent. `internal/podvm` is the contract between the shim and the agent. `internal/podnet` moves the pod network to a TAP device.
- `mise run ci` passes on macOS and Ubuntu. `mise run test:hv` passes on an Apple silicon Mac, in the `hv` job, and in an Ubuntu 24.04 arm64 VM with nested KVM. The `kind` job runs the `k8s-p1` target and the PRD phase 1 criteria. The `kind-p2` job installs phase 1, upgrades the node to phase 2, and runs the `k8s-p2` target, the phase 2 criteria, and the phase 1 criteria that still apply.
- A cached `sbxr run alpine true` takes about 0.14 s on an M4 Pro Mac (median of 20 runs).
- Deviation from the S3 plan: no checked-in fixture root. S3 and S4 landed together, and the `cli` target uses pulled busybox images (PRD "Test contract").

## Open work

- **Published manifests.** `kubectl apply -f https://github.com/albertsko/sbxr/releases/latest/download/sbxr-install.yaml` has not run against a cluster. CI tests manifests that it renders from the same template with a locally built node image.
- **Node distributions.** The installer runs in CI only on kind. k3s, RKE2, k0s, and kubeadm-style nodes are untested end to end. Assumption A4 covers only their drop-in imports.
- **arm64 hosts.** No CI runner for arm64 has KVM. `test:hv` passes in a local Ubuntu 24.04 arm64 VM with nested KVM, but no job runs it. Phase 1 and phase 2 have not run on an arm64 node, and only the release workflow builds the arm64 node image.
- **PRD assumptions A1, A6, and A9** are still open, and open question Q1 (upstream `libkrun` bug reports) waits for the project owner. See PRD "Assumptions and Open Questions".
- **Known limits that stay:** the rare `hvc0` console race can still lose guest output under heavy host load (PRD "Workload config"), terminal resizes reach the guest only on Linux (`libkrun` 1.19.8), and the phase 1 and phase 2 limits in [kubernetes.md](kubernetes.md).

## Development environment

### macOS (Apple silicon, macOS 15 or later)

```sh
brew install libkrun            # also installs libkrunfw
mise trust && mise install
mise run ci
```

- `libkrun` is in `/opt/homebrew/lib`. See the gotcha "Library loading on macOS".
- Every binary that starts a VM, including test binaries, must be signed. See "Signing on macOS".
- Hosted macOS CI runners cannot start VMs. Real-VM tests on macOS run only on a local machine.

### Linux (x86_64 or arm64, with KVM)

- Add your user to the `kvm` group, so that `/dev/kvm` is readable and writable.
- Install `uidmap`, and check that your user has a range in `/etc/subuid` and `/etc/subgid`.
- On Ubuntu 23.10 and later, install the AppArmor profile from PRD "Platforms and dependencies", and install the binary as `/usr/local/bin/sbxr`. Then run the real VM tests against it: `SBXR_BIN=/usr/local/bin/sbxr mise run test:hv`.
- In a Linux VM that shares the checkout with a Mac (for example Lima), do not run `mise run build`, because it overwrites the Mac's `bin/sbxr`. Build to a private path instead: `CGO_ENABLED=0 go build -o /tmp/sbxr ./cmd/sbxr && sudo install -m 755 /tmp/sbxr /usr/local/bin/sbxr`.
- No Ubuntu LTS ships `libkrun`. Build it from source with network support, and use the `libkrunfw` release tarball. Alternative: install both with Homebrew on Linux.

  ```sh
  sudo apt-get install -y libclang-dev
  git clone --depth 1 --branch v1.19.8 https://github.com/libkrun/libkrun
  make -C libkrun NET=1
  sudo make -C libkrun NET=1 install PREFIX=/usr/local LIBDIR_Linux=lib   # installs libkrun.so.1 into /usr/local/lib

  # The libkrunfw tarball unpacks to lib/<triplet>/. Copy the library next to libkrun.
  curl -fsSLO https://github.com/libkrun/libkrunfw/releases/download/v5.6.4/libkrunfw-x86_64.tgz   # arm64: libkrunfw-aarch64.tgz
  mkdir krunfw && tar -xzf libkrunfw-x86_64.tgz -C krunfw
  sudo cp -a krunfw/lib/*/libkrunfw.so* /usr/local/lib/
  sudo ldconfig
  ```

  `sbxr` searches `/usr/local/lib` by default. Set `SBXR_LIB_DIR` only when both libraries are in another directory.

- Hosted `ubuntu-24.04-arm` runners have no KVM. Real-VM tests on Linux arm64 run only on a local machine or a self-hosted runner.

### Signing on macOS

The repository root has this `entitlements.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>com.apple.security.hypervisor</key>
  <true/>
</dict>
</plist>
```

Sign a binary ad hoc, without hardened runtime:

```sh
codesign --entitlements entitlements.plist --force -s - ./bin/sbxr
```

Do not add `--options runtime`. Hardened runtime removes the `DYLD_*` variables at launch, and then `libkrun` cannot find `libkrunfw` (tested). PRD "Distribution" lists the options for a later notarized release.

`scripts/gotest-exec.sh` is a small wrapper for `go test -exec`. It signs the test binary and then runs it, so tests that start real VMs work on macOS. Pass it as an absolute path, because `go test` runs the `-exec` program from the package directory:

```sh
go test -tags hv -exec "$PWD/scripts/gotest-exec.sh" -count=1 ./internal/vm/libkrun/...
```

## Running tests

| Tier                            | Command                           | macOS arm64 (local) | Ubuntu x86_64 | Ubuntu arm64              | CI job                          |
| ------------------------------- | --------------------------------- | ------------------- | ------------- | ------------------------- | ------------------------------- |
| `fake` target, unit, fuzz smoke | `mise run test`                   | yes                 | yes           | yes                       | `check`, `linux-arm64`, `macos` |
| `cli` target (real VM)          | `mise run test:hv`                | yes                 | yes           | local or self-hosted only | `hv` (Ubuntu x86_64)            |
| `k8s-p1` target                 | `SBXR_PHASE=1 mise run test:kind` | no                  | yes           | no                        | `kind`                          |
| `k8s-p2` target                 | `mise run test:kind`              | no                  | yes           | no                        | `kind-p2`                       |

- `mise run fuzz [DURATION]` fuzzes the config writer against the guest init parser copy (default `1m`). The nightly `fuzz` CI job runs it for 15 minutes.
- `mise run test` never needs a hypervisor or external network. Real-VM tests live behind a build tag or in a separate package.
- `test:hv` and `test:kind` skip with exit 0 when no hypervisor exists. With `SBXR_REQUIRE_HV=1`, they fail instead. CI sets this variable.
- `test:hv` and `test:kind` run with `-count=1`, because Go's test cache does not notice changes to the built binary or to `libkrun`.
- `test:kind` needs Docker, kind, and kubectl. It creates the kind cluster `sbxr` from `kind.yaml` or reuses it (`SBXR_KIND_CLUSTER`), builds the installer image unless `SBXR_NODE_IMAGE` names one, installs `sbxr`, and uninstalls it at the end. `SBXR_PHASE` selects the phase (default 2), and `SBXR_UPGRADE_FROM=1` installs phase 1 first and then upgrades, as the `kind-p2` job does. The first local image build compiles `libkrun` and takes about 10 minutes.
- CI setup that a local machine usually does not need:

  ```sh
  # KVM for all users on GitHub runners
  echo 'KERNEL=="kvm", GROUP="kvm", MODE="0666", OPTIONS+="static_node=kvm"' | sudo tee /etc/udev/rules.d/99-kvm4all.rules
  sudo udevadm control --reload-rules && sudo udevadm trigger --name-match=kvm

  # AppArmor profile (Ubuntu 23.10 and later), after installing /usr/local/bin/sbxr
  sbxr doctor apparmor-profile | sudo tee /etc/apparmor.d/sbxr
  sudo apparmor_parser -r /etc/apparmor.d/sbxr

  # kind resets /dev/kvm permissions when it creates a cluster
  kind create cluster --config kind.yaml && sudo chmod 666 /dev/kvm
  ```

## Run first, in parallel

These two spikes can change the design. Run them before the step that depends on them. They need none of the product code.

- **A10, before S3: macOS exit hang (done, passed; see "Evidence").** Write a standalone stress test of the userspace network stack. Hold the per-run connection cap (1024 TCP connections) on macOS, then exit. The process must exit. If it hangs, lower the caps or move the userspace network stack into a separate helper process, which changes the PRD process model.
- **A2, A4, A8, before S7: phase 1 node bundle.** Done. See the assumption rows in the PRD.

## Build plan

Each step ends when its exit criteria pass in CI, or locally where CI cannot run VMs. Every step also meets the "Definition of done". Step S7 shares little code with the CLI and can run in parallel with S5 and S6.

### S1: test harness and core boundaries

Scope:

- Create the core packages and their interfaces (see "Core boundaries").
- Enable `depguard` in `linters.enable` of `.golangci.yml` (the config uses `default: none`). Add a rule that core packages (everything under `internal/` except the CLI adapter and the backend adapters) cannot import cobra, fang, `gvisor-tap-vsock`, or Kubernetes packages.
- Write the target interface from PRD "Test contract": capabilities, error classes, and the cleanup check.
- Write the `fake` target without network capability. The `fake` target gets the network capability in S5.
- Write 3 scenarios: echo, exit code, invalid input.
- Extend `deps` to run `go mod vendor`, and make `ci` call `deps`.
- Add the tasks `test:hv` and `test:kind`. `tasks/test` is a file today, so move it to `tasks/test/_default` (the mise name for a default task) and add `tasks/test/hv` and `tasks/test/kind` next to it.
- Add `kind` and `kubectl` to `[tools]` in `mise.toml`.
- Add the CI jobs `linux-arm64` and `macos` next to `check`. In S1 both run only the `fake` target. S2 adds the help-timing test to `macos`, and S3 adds the codesign and `doctor` checks.

Exit criteria:

- `mise run ci` is green on macOS and Ubuntu.
- A reference target written for the test suite passes all 3 scenarios.
- A meta-test runs the scenarios against a deliberately broken target (wrong exit code, leftover process) and passes only when the scenarios and the cleanup check fail.
- Scenarios that need the `sbxr` binary are skipped with the reason "needs run: S2".
- `test:hv` and `test:kind` skip with exit 0, and fail when `SBXR_REQUIRE_HV=1`.
- PRD acceptance criterion "`mise run test` runs all fake-VM scenarios" under "Tests and CI" passes.

### S2: CLI skeleton and config writer

Scope:

- Add cobra and fang with the stdout workaround and one-line errors (PRD "CLI contract").
- Add input validation.
- Add the config writer, with property and fuzz tests against a copy of the guest init parser.
- Add the nightly `fuzz` CI job.

Exit criteria:

- The S1 scenarios pass on the `fake` target through the `sbxr` binary.
- A test runs `sbxr --help`, an unknown command, and a bad flag on a pseudo-terminal that never answers color queries. Each finishes in under 1 second.
- PRD acceptance criteria under "Output" pass on the `fake` target, except the failed-preflight timing, which S3 covers.
- The config writer rejects every case listed in PRD "Workload config", and the fuzz tests find no input that makes the parser copy disagree with the writer.

### S3: libkrun binding, doctor, first real VM

Scope:

- Add the `libkrun` binding through `purego`, the preflight checks except `userns`, and `sbxr doctor`.
- Add `entitlements.plist` and the signing wrapper.
- Boot a checked-in fixture root filesystem (a static busybox directory). Image pull comes in S4.
- Add the `hv` CI job on Ubuntu x86_64, and a CI job that builds `libkrun` and caches it by version.

Exit criteria:

- A golden test validates `sbxr doctor --json` against a committed JSON schema that matches PRD "`sbxr doctor` contract".
- `mise run test:hv` passes in the `hv` job with `SBXR_REQUIRE_HV=1`.
- `mise run test:hv` passes on a local Mac. Paste its output into the pull request.
- A failed preflight finishes in under 1 second on a pseudo-terminal that never answers color queries.
- PRD acceptance criterion about `mise run test:hv` under "Tests and CI" passes.

### S4: images, root filesystem, user namespace

Scope:

- Add image pull, unpack, and cache, then the per-run root (APFS clone on macOS, overlay on Linux).
- Add the Linux user namespace, the `userns` doctor check, and `sbxr doctor apparmor-profile`.
- Install `sbxr` to `/usr/local/bin` and the AppArmor profile in the `hv` job.

Exit criteria:

- PRD acceptance criteria under "Run a workload (CLI)" and "Signals and interactive use" pass in the `hv` job and on a local Mac.
- PRD acceptance criteria under "Install and preflight" pass, except the two fresh-install criteria (`brew install` and the Ubuntu setup block), which S6 covers.
- The 1-second boot criterion is a local benchmark on a Mac, not a CI gate.

### S5: networking

Scope:

- Add the userspace network stack with `sbxr`'s own TCP and UDP forwarders, the egress guard, the connection caps, and `-p`.
- Add the network capability to the `fake` target.

Exit criteria:

- PRD acceptance criteria under "Networking and mounts" pass on the `fake` target, in the `hv` job, and on a local Mac.
- Publish tests bind port 0, and network tests reach a local test server instead of the public internet.

### S6: distribution

Scope:

- Add the Homebrew tap formula, the Linux setup block, and the goreleaser release.

Exit criteria:

- On a fresh `ubuntu-latest` runner, the documented steps end with `sbxr doctor` exit 0.
- On a fresh `macos-15` runner, `sbxr doctor` fails only the `hypervisor` check, because hosted macOS runners have no hypervisor.
- The two fresh-install criteria under PRD "Install and preflight" pass.
- These runs also settle assumption A5 (Homebrew tap trust).

Done. Evidence: `install` run [43722622849](https://github.com/albertsko/sbxr/actions/runs/43722622849) on fresh runners (exit criteria and A5), and release run [43830749255](https://github.com/albertsko/sbxr/actions/runs/43830749255) for v0.1.0. Against the published release: on an Apple silicon Mac, `brew install albertsko/tap/sbxr` installed 0.1.0, `sbxr doctor` exited 0, and `run`, `-v`, and `--net` worked; in an Ubuntu 24.04 arm64 VM, the README download line fetched the archive, the checksum matched, and the binary reported 0.1.0.

### S7: Kubernetes phase 1

Done. Evidence: `kind` job run [43723330626](https://github.com/albertsko/sbxr/actions/runs/43723330626).

Scope:

- Add the node bundle, the installer DaemonSet, the admission policies, and the `kind` CI job.
- Commit the kind cluster config (`kind.yaml`) with the `/dev/kvm` mount and the containerd patch that imports the `sbxr` drop-in directory.
- Measure the RuntimeClass overhead (open question Q2).

Exit criteria:

- PRD acceptance criteria under "Kubernetes, phase 1" pass in the `kind` job.
- The measured overhead value is in the PRD, and Q2 is removed from the open questions.
- PRD acceptance criterion about `mise run test:kind` under "Tests and CI" passes.

### S8: Kubernetes phase 2

Done. Evidence: `kind-p2` job in run [43751384434](https://github.com/albertsko/sbxr/actions/runs/43751384434). Spike A7: run [43727468549](https://github.com/albertsko/sbxr/actions/runs/43727468549), tag `archive/spike-k8s-p2`.

Scope:

- Add the shim, the guest agent, and TAP networking. Add a `kind-p2` CI job for the `k8s-p2` target.

Exit criteria:

- PRD acceptance criteria under "Kubernetes, phase 2" pass in the `kind-p2` job.

## Core boundaries

The PRD requires a core with no CLI, Kubernetes, or network-stack code (user story 44). Create these packages in S1 and S2. Names are suggestions. The import rule is not.

- **Run spec and validation:** the run request and every input check. No I/O.
- **Lifecycle runner:** the run states from PRD "Run lifecycle" and the cleanup guarantee.
- **VM backend interface:** with a `fake` adapter (S1) and a `libkrun` adapter (S3).
- **Network backend interface:** with the userspace network stack adapter (S5). The phase 2 TAP network lives outside the core, in `internal/podnet`.
- **Image cache and root filesystem:** pull, unpack, per-run root.
- **Preflight:** checks shared by `doctor` and `run`.
- **Config writer.**
- **CLI adapter:** cobra, fang, and output formatting. Only this package imports cobra and fang.

## Definition of done

A step or pull request is done when:

- `mise run ci` passes.
- The pull request title follows Conventional Commits (see AGENTS.md).
- Tests describe behavior, not implementation details.
- The PRD is updated in the same pull request when behavior, a contract, or an assumption changes.

## Changing a decision

- To reopen a decision, open a GitHub issue that names the PRD section, the new evidence, and the proposed change. The project owner (repository owner `albertsko`) approves.
- After approval, change the PRD section in the same pull request as the code. Remove a resolved open question, and move the answer into the matching section.
- Record a failed assumption by updating its row in PRD "Assumptions" and the sections it affects.
- Open question Q1 (filing `libkrun` bugs) is the project owner's decision. Record the outcome in PRD "Open questions".

## Gotchas

- **The node status is a poor signal for "containerd serves the handler".** With node leases, the kubelet posts an unchanged node status only every `nodeStatusReportFrequency` plus a random delay (2.5 to 7.5 minutes by default), and it may post handlers from its cache. The installer asks containerd through the CRI `Status` call instead.
- **`libkrun` takes over its process.** `krun_start_enter` exits the process with the workload's exit code. Start each VM in its own VM process.
- **Library loading on macOS.** `libkrun` loads `libkrunfw` by bare name. Start the VM process with `DYLD_FALLBACK_LIBRARY_PATH` set to the library directory. Setting it inside a running process is too late. macOS also removes `DYLD_*` variables when a protected binary such as `/usr/bin/env` or `/usr/bin/time` starts the program.
- **Never pass `NULL`** to `krun_set_env`, which copies the host environment and can panic on a large one, or to `krun_set_port_map`, which crashes.
- **Do not call `krun_set_exec`, `krun_set_workdir`, or `krun_set_rlimits`.** These functions put values on the kernel command line, which breaks on quotes and non-ASCII text and overrides the config file.
- **The guest init parser matches values as keys.** Follow PRD "Workload config" exactly, including the key order.
- **The workload is not PID 1** under the `libkrun` built-in init, so a signal that the workload sends to itself can end it.
- **TSI exposes every guest listener on the host, and no setting limits it.** The CLI never uses TSI. "No network" mode disables socket forwarding and adds no network device. Phase 1 Kubernetes uses TSI inside the pod network namespace.
- **The userspace network stack dials any destination.** In testing, guest UDP to `0.0.0.0` reached a host loopback service. Do not ship `--net` without the egress guard.
- **Go's built-in user namespace mapping writes only one mapping.** Use `newuidmap` and `newgidmap`, and re-execute only after the maps are written. The execve as mapped uid 0 also clears the parent death signal, so `internal/userns` sets it again.
- **containerd's shim sandboxer ignores `runtime_path`.** It finds `containerd-shim-sbxr-v1` only in `PATH` or next to the containerd binary, so the installer links it into `/usr/local/bin`.
- **containerd waits for EOF on a process's output FIFOs** before it deletes the process. The shim holds the only write ends, so it closes them when the guest stream ends, or 2 seconds after the exit when a child process keeps the output open.
- **Never delete a bundle that a VM may use.** A phase 2 VM's guest root and the agent's `crun` live in the bundle, and `libkrun` holds the root by an `O_PATH` descriptor, so a rename is safe and a removal breaks exec, probes, and stop. The installer stages each new bundle below `/opt/sbxr.old` and exchanges it with `/opt/sbxr`, so the old bundle is retired in the same step, and removes a retired bundle only when no process in `/proc` uses it. The VM process reaches its bundle, its own binary included, only through a pinned descriptor (`/proc/self/fd/3`). The installer pod sees host paths both as `/opt/...` and as `/host/opt/...` in `/proc`, so it checks both.
- **Overlayfs cannot be idmapped,** and separate idmapped binds of its upper and work directories fail ("must reside under the same mount"). Idmap only the lower layers. Copy-up keeps their mapped owners.
- **Go's `net.FileListener` rejects vsock sockets.** The agent accepts on the raw socket with non-blocking descriptors, so connections still use the runtime poller.
- **The guest agent is PID 1** and reaps every process. It never calls `exec.Cmd.Wait`. `reboot(RESTART)` ends the VM, `poweroff` hangs it.
- **kind resets `/dev/kvm` permissions** when it creates a cluster. Run `sudo chmod 666 /dev/kvm` after `kind create cluster`.
- **kind needs D-Bus and a version 2 drop-in.** With systemd cgroups, `crun` reaches systemd only through D-Bus, and the kind node image has none, so the kind setup installs `dbus` in the node. kind writes a version 2 containerd config, and containerd fails to start with a version 3 drop-in under it.
- **Kubernetes rewrites `$$` to `$`** in pod commands. Write `$$$$` in test pod specs.
- **`kind load docker-image` fails for images with attestations or several platforms.** The kind setup rebuilds the public test images locally (`FROM busybox:1.37`) and turns off default attestations. The scenario images reach kind through `kind load image-archive` from single-platform tarballs that the kind test writes, so do not add them to `kind-up.sh`.
- **containerd ignores the group of a numeric image `USER`** (`1000:1000` runs with gid 0) unless the pod sets `runAsGroup`. Kubernetes targets therefore lack the `exact-config` capability.
- **fang queries the terminal color on every help and error render.** The workaround in PRD "Further Notes" keeps workload output safe only if stdout is restored before any workload runs. Restore it inside the `run` command, not in a persistent hook, because hooks also run for `sbxr help` and would bring the color query back.
- **The race detector needs cgo.** The release binary builds with cgo off. Build the binary that the `cli` target tests with `CGO_ENABLED=0`, the same as the release.
- **The `-t` terminal is not the VM process's controlling terminal.** When a controlling process exits, the kernel hangs the terminal up, and macOS then drops unread output. Without `-i`, the VM process's stdin is empty, and `libkrun` still sets up its console from stdout (checked with real VMs). Resizes do not signal the VM process by themselves, so the parent sends it SIGWINCH.
- **`libkrun` 1.19.8 forwards terminal resizes only on Linux.** On macOS the guest keeps the size from the start.
- **Guest output can get lost under heavy host load.** See the console race in PRD "Workload config". Real-VM tests run with `-parallel 4` to stay far below that load.
- **`newgidmap` maps only the user's primary group.** Under `sg kvm` the process gid differs and the `userns` check fails. Log in again after `usermod -aG kvm` (or use `sudo -iu "$USER"`) instead.
- **On Linux, image cache files belong to subordinate ids.** Remove a cache directory with `sbxr prune --force` (with `SBXR_CACHE_DIR` set), not `rm -rf`. Tests may leave such directories in the temporary directory.
- **CI shuffles and parallelizes tests.** Publish tests must bind port 0. Cleanup checks must look only at the test's own run directory and processes, not at every process on the host.

## Evidence

Each claim in PRD "Verification log" comes from one of these GitHub Actions runs in `albertsko/sbxr`, or from local runs on an Apple silicon Mac. The local runs left no logs in the repository.

The experiment branches are deleted. Each experiment commit has a tag `archive/spike-<name>`, so its code stays available. For example:

```sh
git fetch origin tag archive/spike-gvnet
git show archive/spike-gvnet:.github/workflows/gvnet.yml
```

| Topic                                                                                   | Run                                                                       | Tag                             |
| --------------------------------------------------------------------------------------- | ------------------------------------------------------------------------- | ------------------------------- |
| KVM and kind on GitHub runners, `libkrun` CI install                                    | [43411616835](https://github.com/albertsko/sbxr/actions/runs/43411616835) | `archive/spike-ci-probe`        |
| Rootless ownership in a user namespace                                                  | [43411920655](https://github.com/albertsko/sbxr/actions/runs/43411920655) | `archive/spike-q5-ownership`    |
| AppArmor profile for user namespaces                                                    | [43413103721](https://github.com/albertsko/sbxr/actions/runs/43413103721) | `archive/spike-a4-apparmor`     |
| Phase 2 TAP networking                                                                  | [43413531258](https://github.com/albertsko/sbxr/actions/runs/43413531258) | `archive/spike-a8-tap`          |
| One `NET=1` build behaves like the default build                                        | [43418374705](https://github.com/albertsko/sbxr/actions/runs/43418374705) | `archive/spike-net1-parity`     |
| Userspace network stack in the `sbxr` process                                           | [43421578807](https://github.com/albertsko/sbxr/actions/runs/43421578807) | `archive/spike-gvnet`           |
| Phase 1 `crun` with `libkrun` in kind                                                   | [43414830267](https://github.com/albertsko/sbxr/actions/runs/43414830267) | `archive/spike-a5-kind-krun`    |
| Phase 1 error reporting                                                                 | [43423090034](https://github.com/albertsko/sbxr/actions/runs/43423090034) | `archive/spike-k8s-errors`      |
| Per-run root on Linux                                                                   | [43425445163](https://github.com/albertsko/sbxr/actions/runs/43425445163) | `archive/spike-per-run-root`    |
| Phase 1 DNS mitigation                                                                  | [43426355714](https://github.com/albertsko/sbxr/actions/runs/43426355714) | `archive/spike-k8s-dns`         |
| Egress guard and connection caps                                                        | [43593357284](https://github.com/albertsko/sbxr/actions/runs/43593357284) | `archive/spike-netstack-bypass` |
| Phase 1 node bundle, drop-in directories, MutatingAdmissionPolicy versions (A2, A4, A8) | [43704817204](https://github.com/albertsko/sbxr/actions/runs/43704817204) | `archive/spike-node-bundle`     |
| Fresh install on Ubuntu and macOS, tap trust (A5)                                       | [43722622849](https://github.com/albertsko/sbxr/actions/runs/43722622849) | none                            |
| Phase 2 guest agent, vsock, TAP, idmapped container roots (A7)                          | [43727468549](https://github.com/albertsko/sbxr/actions/runs/43727468549) | `archive/spike-k8s-p2`          |

- **A10, macOS exit with held connections** (local, M4 Pro, `libkrun` 1.19.8, 2026-11-15, no tag): a stress build of the network stack with its own forwarders held guest connections in a real VM, then the VM exited, was killed, or the parent got SIGINT. 5 runs per case at the caps (1024 TCP, 256 UDP), above them (1100 TCP, 300 UDP), and with 2000 TCP connections. The parent exited within 0.1 s every time, and no process stayed stuck.

GitHub keeps Actions logs for a limited time (90 days by default), so the run links expire. The tags keep the code. Copy any log output you need into an issue or the repository before the links expire.
