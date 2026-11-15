<div align="center"><h3>sbxr</h3></div>

`sbxr` runs an OCI container image as a small virtual machine with its own Linux kernel, using `libkrun`. `sbxr run alpine echo hi` behaves like `docker run`, but the workload runs in a microVM on macOS arm64 or Linux with KVM, without root.

## Status

Early development. `sbxr run`, `sbxr doctor`, and `sbxr prune` work with real VMs on macOS arm64 and on Linux with KVM, without root, and with images pulled from registries. The Kubernetes RuntimeClass works in phase 1 ([docs/kubernetes.md](docs/kubernetes.md)). Networking (`--net`, `-p`) and volumes (`-v`) come next. See [docs/PRD.md](docs/PRD.md) for the requirements and [docs/HANDOFF.md](docs/HANDOFF.md) for the build plan.

## Install

### macOS

Apple silicon with macOS 15 or later:

```sh
brew install albertsko/tap/sbxr
sbxr doctor
```

The formula installs `libkrun` from Homebrew and signs `sbxr` with the hypervisor entitlement.

### Linux

Ubuntu 24.04 on x86_64 or arm64 with KVM and [Homebrew on Linux](https://docs.brew.sh/Homebrew-on-Linux). `sbxr doctor` prints the same block when a check fails.

```sh
sudo apt-get install -y uidmap
grep -q "^$USER:" /etc/subuid || sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 "$USER"
sudo usermod -aG kvm "$USER"
brew install libkrun
curl -fsSL "https://github.com/albertsko/sbxr/releases/latest/download/sbxr_linux_$(dpkg --print-architecture).tar.gz" | tar -xz sbxr
sudo install -m 755 sbxr /usr/local/bin/sbxr && rm sbxr
sbxr doctor apparmor-profile | sudo tee /etc/apparmor.d/sbxr >/dev/null
sudo apparmor_parser -r /etc/apparmor.d/sbxr
# Log in again so that the kvm group applies, then run: sbxr doctor
```

The AppArmor profile covers only `/usr/bin/sbxr` and `/usr/local/bin/sbxr`, so install `sbxr` to one of these paths.

## Build and test

```sh
mise trust && mise install
mise run deps        # vendor Go modules
mise run build       # bin/sbxr
mise run test        # unit tests and fake VM scenarios, no hypervisor needed
mise run test:hv     # scenarios against a real VM, skipped without a hypervisor
mise run test:kind   # Kubernetes phase 1 in kind (Linux with KVM and Docker)
mise run ci          # everything CI runs
```
