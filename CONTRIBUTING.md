# Contributing

[AGENTS.md](AGENTS.md) has the repository rules for tasks, commits, and releases, and the project layout. Run `mise run ci` before you consider a change complete.

## Setup on macOS

Apple silicon with macOS 15 or later:

```sh
brew install libkrun            # also installs libkrunfw
mise trust && mise install
mise run ci
```

### Signing

Every binary that starts a VM, including test binaries, must be signed ad hoc with the hypervisor entitlement in [cmd/sbxr/entitlements.plist](cmd/sbxr/entitlements.plist). `mise run build` signs `bin/sbxr`. To sign another binary:

```sh
codesign --entitlements cmd/sbxr/entitlements.plist --force -s - ./bin/sbxr
```

Do not add `--options runtime`. Hardened runtime removes the `DYLD_*` variables at launch, and then `libkrun` cannot find `libkrunfw`. For the same reason, do not start `sbxr` through a protected system binary such as `/usr/bin/env` or `/usr/bin/time`.

`scripts/gotest-exec.sh` signs a test binary and then runs it. `mise run test:vm` uses it. Pass it as an absolute path, because `go test` runs the `-exec` program from the package directory:

```sh
go test -tags vm -exec "$PWD/scripts/gotest-exec.sh" -count=1 -run '^TestVM' ./internal/libkrun/...
```

## Setup on Linux

x86_64 or arm64 with KVM:

- Add your user to the `kvm` group, then log in again. `newgidmap` maps only the primary group from `/etc/passwd`, so `sg kvm` or `newgrp` makes the `userns` check fail.
- Install `uidmap`, and check that your user has a range in `/etc/subuid` and `/etc/subgid`.
- On Ubuntu 23.10 and later, install the binary as `/usr/local/bin/sbxr` and the AppArmor profile (see `sbxr doctor`). Then run the real VM tests against that binary: `SBXR_BIN=/usr/local/bin/sbxr mise run test:vm`.
- In a Linux VM that shares the checkout with a Mac (for example Lima), do not run `mise run build`, because it overwrites the Mac's `bin/sbxr`. Build to a private path instead: `CGO_ENABLED=0 go build -o /tmp/sbxr ./cmd/sbxr && sudo install -m 755 /tmp/sbxr /usr/local/bin/sbxr`.
- Install `libkrun` and `libkrunfw` with Homebrew on Linux, or build `libkrun` from source with network support and use the `libkrunfw` release tarball:

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

## Targets and hosts

Tests run against real environments, with no fake targets or simulated VMs. A unit test may replace an outside system only to inject failures that a real environment cannot produce. A test is a unit test, or it runs against one target: `vm` (a real libkrun VM) or `kube` (Kubernetes in a kind cluster). The host is `darwin` or `linux`.

1. Put host-specific code in `_darwin.go` or `_linux.go` files, or behind a `GOOS` build constraint. Do not invent build tags for hosts.
2. Write a unit test as a plain `_test.go` file without a build tag. It needs no hypervisor and no network. `mise run test` runs it everywhere.
3. Put a `vm` test in `<topic>_vm_test.go` with `//go:build vm`, and name it `TestVM...`. `mise run test:vm` runs only `^TestVM`.
4. Put a `kube` test in `<topic>_kube_test.go` with `//go:build kube`, and name it `TestKube...`. `mise run test:kube` runs only `^TestKube`.
5. When the environment of a `vm` or `kube` test is missing, call `testenv.SkipOrFailf`. It skips, or fails when `SBXR_TEST_REQUIRE=1`. `internal/testenv` checks rules 3 and 4.

## Tests

| Command                | Runs                                                                | Needs                                                                 |
| ---------------------- | ------------------------------------------------------------------- | --------------------------------------------------------------------- |
| `mise run test`        | Unit tests and the host-only CLI tests of the release binary        | Nothing: no hypervisor, no network                                    |
| `mise run test:vm`     | The scenarios, the CLI tests, and the `libkrun` tests with real VMs | A hypervisor, on macOS a signed binary, and access to `mirror.gcr.io` |
| `mise run test:kube`   | The scenarios and the Kubernetes tests in a kind cluster            | Linux with KVM, Docker, kind, kubectl                                 |
| `mise run fuzz [TIME]` | Fuzzing of the config writer against the guest init parser          | Nothing. Default duration `1m`                                        |

- `test:vm` and `test:kube` skip with exit 0 when their environment is missing: no hypervisor, or for `test:kube` no Linux. With `SBXR_TEST_REQUIRE=1`, they fail instead. CI sets this variable.
- `test:vm` and `test:kube` run with `-count=1`, because Go's test cache does not notice changes to the built binary or to `libkrun`.
- `test:kube` runs [scripts/kind/up.sh](scripts/kind/up.sh), which creates the kind cluster `sbxr` from [scripts/kind/cluster.yaml](scripts/kind/cluster.yaml), or reuses the cluster named by `SBXR_KIND_CLUSTER`. It builds the installer image unless `SBXR_NODE_IMAGE` names one. The first image build compiles `libkrun` and takes several minutes.
- kind resets the `/dev/kvm` permissions when it creates a cluster. Run `sudo chmod 666 /dev/kvm` after `kind create cluster`.
- `mise run build` runs `go generate ./internal/guestmount`, which builds the guest mount helper that `-v` needs. A bare `go build` embeds whatever helper is on disk, and without one `-v` fails with exit code 125.
- `internal/e2e` holds the end-to-end tests. They build the release binary themselves, and the `vm` tests build their images on `mirror.gcr.io/library/busybox`.
- `mise run test:vm` passes extra arguments to `go test`, for example `mise run test:vm -v -shuffle=on`.
- To run the `vm` tests on Linux, install `sbxr` and the AppArmor profile as in [Setup on Linux](#setup-on-linux), then run `SBXR_TEST_REQUIRE=1 SBXR_BIN=/usr/local/bin/sbxr mise run test:vm`.
- Tests run in parallel, and the nightly workflow also shuffles them, including the `vm` tests on Linux KVM. Publish tests must bind port 0, and cleanup checks must look only at the test's own run directory and processes.
- Hosted macOS and Linux arm64 runners have no usable hypervisor, so CI runs the `vm` tests only on Linux x64 with KVM.
- Before each release, run `SBXR_TEST_REQUIRE=1 mise run test:vm` on an Apple silicon Mac. This local run is the macOS release gate.
