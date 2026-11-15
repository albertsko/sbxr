<div align="center"><h3>sbxr</h3></div>

`sbxr` runs an OCI container image as a small virtual machine with its own Linux kernel, using `libkrun`. `sbxr run alpine echo hi` behaves like `docker run`, but the workload runs in a microVM on macOS arm64 or Linux with KVM, without root.

## Status

Early development. `sbxr run`, `sbxr doctor`, and `sbxr prune` work with real VMs on macOS arm64 and on Linux with KVM, without root, and with images pulled from registries. The Kubernetes RuntimeClass works in phase 1 ([docs/kubernetes.md](docs/kubernetes.md)). Networking (`--net`, `-p`) and volumes (`-v`) come next. See [docs/PRD.md](docs/PRD.md) for the requirements and [docs/HANDOFF.md](docs/HANDOFF.md) for the build plan.

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
