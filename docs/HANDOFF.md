# sbxr: Handoff

Status: research and design are complete, and no product code exists yet. The PRD stays a living document. Last update: 2026-11-14.

**Next: start step S1, together with the two spikes in "Run first, in parallel".**

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

- The repository comes from the template `albertsko/go-kickstart`. It has one placeholder command in `cmd/sbxr`, mise tasks in `tasks/`, strict golangci-lint, goreleaser, and one CI workflow (`.github/workflows/check.yml`).
- `mise run ci` passes on `main`.
- The Go module has no dependencies yet.
- `vendor/` is gitignored. The existing `deps` task only checks tools. S1 extends it to run `go mod vendor`.

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
- On Ubuntu 23.10 and later, install the AppArmor profile from PRD "Platforms and dependencies", and install the binary as `/usr/local/bin/sbxr`.
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

S3 adds this file as `entitlements.plist` at the repository root:

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

S3 also adds a small wrapper script for `go test -exec`. It signs the test binary and then runs it, so tests that start real VMs work on macOS.

## Running tests

| Tier                            | Command              | macOS arm64 (local) | Ubuntu x86_64 | Ubuntu arm64              | CI job                          |
| ------------------------------- | -------------------- | ------------------- | ------------- | ------------------------- | ------------------------------- |
| `fake` target, unit, fuzz smoke | `mise run test`      | yes                 | yes           | yes                       | `check`, `linux-arm64`, `macos` |
| `cli` target (real VM)          | `mise run test:hv`   | yes                 | yes           | local or self-hosted only | `hv` (Ubuntu x86_64)            |
| `k8s-p1` target                 | `mise run test:kind` | no                  | yes           | no                        | `kind`                          |

- `mise run test` never needs a hypervisor or external network. Real-VM tests live behind a build tag or in a separate package.
- `test:hv` and `test:kind` skip with exit 0 when no hypervisor exists. With `SBXR_REQUIRE_HV=1`, they fail instead. CI sets this variable.
- `test:hv` and `test:kind` run with `-count=1`, because Go's test cache does not notice changes to the built binary or to `libkrun`.
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

- **A10, before S3: macOS exit hang.** Write a standalone stress test of the userspace network stack. Hold the per-run connection cap (1024 TCP connections) on macOS, then exit. The process must exit. If it hangs, lower the caps or move the userspace network stack into a separate helper process, which changes the PRD process model.
- **A2, A4, A8, before S7: phase 1 node bundle.** Build `crun` with `libkrun` on an old glibc baseline (for example Ubuntu 22.04, glibc 2.35), bundle its libraries, and run it on Ubuntu 22.04, Debian 12, and the kind node image. Read the rendered containerd config on an RKE2 node. Run `kubectl api-resources` for MutatingAdmissionPolicy on the oldest Kubernetes version you plan to support.

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

### S7: Kubernetes phase 1

Scope:

- Add the node bundle, the installer DaemonSet, the admission policies, and the `kind` CI job.
- Commit the kind cluster config (`kind.yaml`) with the `/dev/kvm` mount and the containerd patch that imports the `sbxr` drop-in directory.
- Measure the RuntimeClass overhead (open question Q2).

Exit criteria:

- PRD acceptance criteria under "Kubernetes, phase 1" pass in the `kind` job.
- The measured overhead value is in the PRD, and Q2 is removed from the open questions.
- PRD acceptance criterion about `mise run test:kind` under "Tests and CI" passes.

### S8: Kubernetes phase 2

Scope:

- Add the shim, the guest agent, and TAP networking. Add a `kind-p2` CI job for the `k8s-p2` target.

Exit criteria:

- PRD acceptance criteria under "Kubernetes, phase 2" pass in the `kind-p2` job.

## Core boundaries

The PRD requires a core with no CLI, Kubernetes, or network-stack code (user story 44). Create these packages in S1 and S2. Names are suggestions. The import rule is not.

- **Run spec and validation:** the run request and every input check. No I/O.
- **Lifecycle runner:** the run states from PRD "Run lifecycle" and the cleanup guarantee.
- **VM backend interface:** with a `fake` adapter (S1) and a `libkrun` adapter (S3).
- **Network backend interface:** with the userspace network stack adapter (S5) and later the TAP adapter (S8).
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

- **`libkrun` takes over its process.** `krun_start_enter` exits the process with the workload's exit code. Start each VM in its own VM process.
- **Library loading on macOS.** `libkrun` loads `libkrunfw` by bare name. Start the VM process with `DYLD_FALLBACK_LIBRARY_PATH` set to the library directory. Setting it inside a running process is too late. macOS also removes `DYLD_*` variables when a protected binary such as `/usr/bin/env` or `/usr/bin/time` starts the program.
- **Never pass `NULL`** to `krun_set_env`, which copies the host environment and can panic on a large one, or to `krun_set_port_map`, which crashes.
- **Do not call `krun_set_exec`, `krun_set_workdir`, or `krun_set_rlimits`.** These functions put values on the kernel command line, which breaks on quotes and non-ASCII text and overrides the config file.
- **The guest init parser matches values as keys.** Follow PRD "Workload config" exactly, including the key order.
- **The workload is not PID 1** under the `libkrun` built-in init, so a signal that the workload sends to itself can end it.
- **TSI exposes every guest listener on the host, and no setting limits it.** The CLI never uses TSI. "No network" mode disables socket forwarding and adds no network device. Phase 1 Kubernetes uses TSI inside the pod network namespace.
- **The userspace network stack dials any destination.** In testing, guest UDP to `0.0.0.0` reached a host loopback service. Do not ship `--net` without the egress guard.
- **Go's built-in user namespace mapping writes only one mapping.** Use `newuidmap` and `newgidmap`, and re-execute the VM process only after the maps are written.
- **kind resets `/dev/kvm` permissions** when it creates a cluster. Run `sudo chmod 666 /dev/kvm` after `kind create cluster`.
- **The spike built the phase 1 node bundle inside the kind node image** (`kindest/node:v1.37.0`, Debian with glibc 2.41). Such a build does not run on nodes with an older glibc. The product bundle needs an old glibc baseline (assumption A2). `crun` also needs `libjson-c`, which the kind node image lacks.
- **Kubernetes rewrites `$$` to `$`** in pod commands. Write `$$$$` in test pod specs.
- **fang queries the terminal color on every help and error render.** The workaround in PRD "Further Notes" keeps workload output safe only if stdout is restored before any workload runs. Set `cobra.EnableTraverseRunHooks = true`, so that a subcommand hook cannot skip the restore.
- **The race detector needs cgo.** The release binary builds with cgo off. Build the binary that the `cli` target tests with `CGO_ENABLED=0`, the same as the release.
- **CI shuffles and parallelizes tests.** Publish tests must bind port 0. Cleanup checks must look only at the test's own run directory and processes, not at every process on the host.

## Evidence

Each claim in PRD "Verification log" comes from one of these GitHub Actions runs in `albertsko/sbxr`, or from local runs on an Apple silicon Mac. The local runs left no logs in the repository.

The experiment branches are deleted. The code is still reachable by commit SHA. For example:

```sh
gh api 'repos/albertsko/sbxr/contents/.github/workflows/gvnet.yml?ref=f628b68e1a5e' -q .content | base64 -d
```

| Topic                                                | Run                                                                       | Commit         |
| ---------------------------------------------------- | ------------------------------------------------------------------------- | -------------- |
| KVM and kind on GitHub runners, `libkrun` CI install | [43411616835](https://github.com/albertsko/sbxr/actions/runs/43411616835) | `2bdd51d534aa` |
| Rootless ownership in a user namespace               | [43411920655](https://github.com/albertsko/sbxr/actions/runs/43411920655) | `3dc06155afb1` |
| AppArmor profile for user namespaces                 | [43413103721](https://github.com/albertsko/sbxr/actions/runs/43413103721) | `0f1c94fe9f2d` |
| Phase 2 TAP networking                               | [43413531258](https://github.com/albertsko/sbxr/actions/runs/43413531258) | `9055c8c3df5c` |
| One `NET=1` build behaves like the default build     | [43418374705](https://github.com/albertsko/sbxr/actions/runs/43418374705) | `205cb798cb50` |
| Userspace network stack in the `sbxr` process        | [43421578807](https://github.com/albertsko/sbxr/actions/runs/43421578807) | `f628b68e1a5e` |
| Phase 1 `crun` with `libkrun` in kind                | [43414830267](https://github.com/albertsko/sbxr/actions/runs/43414830267) | `84bfd2c0ae89` |
| Phase 1 error reporting                              | [43423090034](https://github.com/albertsko/sbxr/actions/runs/43423090034) | `36d3661dff76` |
| Per-run root on Linux                                | [43425445163](https://github.com/albertsko/sbxr/actions/runs/43425445163) | `70cf3bc8bdf7` |
| Phase 1 DNS mitigation                               | [43426355714](https://github.com/albertsko/sbxr/actions/runs/43426355714) | `245119454a5f` |
| Egress guard and connection caps                     | [43593357284](https://github.com/albertsko/sbxr/actions/runs/43593357284) | `f38c5043a4ff` |

The evidence can disappear:

- GitHub keeps Actions logs for a limited time (90 days by default).
- GitHub can garbage-collect commits that no branch or tag references.

To keep the code, the project owner can push one tag per commit (for example `archive/spike-gvnet`). Copy anything you need into the repository before it expires.
