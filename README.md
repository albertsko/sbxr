<div align="center"><h3>sbxr</h3></div>

`sbxr` runs an OCI container image as a small virtual machine with its own Linux kernel, using `libkrun`. `sbxr run alpine echo hi` behaves like `docker run`, but the workload runs in a microVM on macOS arm64 or Linux with KVM, without root.

## Status

Early development. `sbxr run`, `sbxr doctor`, and `sbxr prune` work with real VMs on macOS arm64 and on Linux with KVM, without root, and with images pulled from registries. The Kubernetes RuntimeClass works in phase 1 ([docs/kubernetes.md](docs/kubernetes.md)). `--net`, `--allow-lan`, and `-p` give the VM a guarded userspace network. Volumes (`-v`) come next. See [docs/PRD.md](docs/PRD.md) for the requirements and [docs/HANDOFF.md](docs/HANDOFF.md) for the build plan.

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
sudo apt-get update && sudo apt-get install -y uidmap
for f in /etc/subuid /etc/subgid; do grep -q "^$USER:" "$f" || echo "$USER:$(awk -F: '{e=$2+$3} e>m{m=e} END{print (m<100000)?100000:m}' "$f"):65536" | sudo tee -a "$f" >/dev/null; done
sudo usermod -aG kvm "$USER"
brew install libkrun
d="$(mktemp -d)" && a="sbxr_linux_$(dpkg --print-architecture).tar.gz" && u=https://github.com/albertsko/sbxr/releases/latest/download
curl -fsSLo "$d/$a" "$u/$a" && curl -fsSLo "$d/checksums.txt" "$u/checksums.txt" && (cd "$d" && sha256sum -c --ignore-missing checksums.txt) && tar -xzf "$d/$a" -C "$d" sbxr && sudo install -m 755 "$d/sbxr" /usr/local/bin/sbxr && rm -r "$d"
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
