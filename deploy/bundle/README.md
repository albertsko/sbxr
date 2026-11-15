# Phase 1 node bundle

`crun` built with `libkrun` support, plus `libkrun` (network support on), `libkrunfw`, and every non-glibc library they load.

## Build

```sh
docker build -f deploy/bundle/Dockerfile --output type=local,dest=out deploy/bundle
```

- Versions are build arguments: `LIBKRUN_VERSION` (`v1.19.8`), `LIBKRUNFW_VERSION` (`v5.6.4`), `CRUN_VERSION` (`1.31`), `PREFIX` (`/opt/sbxr`).
- The build runs on Ubuntu 22.04, so the bundle needs glibc 2.35 or later.
- The build targets the host architecture (x86_64 or arm64).
- `collect.sh` fails the build when any library outside the bundle, other than glibc, is still needed.

## Layout

```text
PREFIX/bin/crun     crun, RUNPATH set to PREFIX/lib
PREFIX/bin/krun     link to crun; crun runs containers in a VM when its program name is krun
PREFIX/lib/         libkrun.so.1, libkrunfw.so.5, libjson-c, libsystemd, libseccomp, libcap, and their dependencies
```

- The bundle works only when installed at `PREFIX`. `crun` re-executes itself from a sealed memfd, where `$ORIGIN` resolves to `/`, so its RUNPATH is absolute. The libraries use `$ORIGIN`.
- Size: 33 MiB unpacked, 13 MiB as `tar.gz` (x86_64). `libkrunfw` is 25 MiB of that.

## Node requirements

- glibc 2.35 or later.
- `/dev/kvm`.
- With `SystemdCgroup = true` (kubelet `cgroupDriver: systemd`), a D-Bus system socket at `/run/dbus/system_bus_socket`. `crun` reaches systemd only through D-Bus, unlike `runc`, which falls back to `/run/systemd/private`. The kind node image has no D-Bus.

## Smoke test

`smoke.sh <bundle-dir> <rootfs-dir>` runs `sh -c "echo hi; exit 3"` from a busybox root in a VM through `krun`, with no containerd. It needs root and `/dev/kvm`.
