# Node installer image

The image behind `sbxr-install.yaml` and `sbxr-uninstall.yaml`. It holds the node bundle under `/bundle` and the `sbxr-node` installer (`cmd/sbxr-node`, `internal/node`). The bundle holds the shim, the guest root, `libkrun`, and `libkrunfw`. See [docs/kubernetes.md](../../docs/kubernetes.md) for the installer steps.

## Build

```sh
docker build -f deploy/node/Dockerfile -t sbxr-node .                                     # installer image
docker build -f deploy/node/Dockerfile --target bundle --output type=local,dest=out .     # bundle only
scripts/node-manifests.sh sbxr-node out                                                   # out/sbxr-install.yaml, out/sbxr-uninstall.yaml
```

- Every input is pinned: base images, the Go image, and the Rust toolchain image by digest, `libkrun` and `libkrunfw` by tag and commit, and the `crun` and `libkrunfw` tarballs and the static guest `crun` by SHA-256 (`fetch.sh`). To update a version, update its pins in the `ARG` lines together.
- The bundle carries the license texts under `share/licenses/` (Go module licenses under `share/licenses/go/`) and a `NOTICE` with versions, sources, and the one change to `libkrun`.
- The bundle builds on Ubuntu 22.04, so it needs glibc 2.35 or later on the node.
- The build targets the host architecture (x86_64 or arm64).
- `collect.sh` fails the build when any library outside the bundle, other than glibc, is still needed.
- `deploy/node/sbxr.yaml` is the manifest template. Releases render it with the image pinned by digest.
- `assemble.sh` adds the shim and guest root and writes `SHA256SUMS`.

## Release

- The release workflow builds the image once per architecture from the tagged commit, pushes each by digest, and joins them in one index tagged with the release version. The `kind` job tests an image built from the same Dockerfile and pins, not the published bytes.
- The index tag is pushed before goreleaser creates the GitHub release. If a later job fails, the tag exists without a release. Rerun the workflow; it pushes the index again and renders the manifests for its new digest.
- The uninstall guard scans `/run/sbxr/pods` and keeps the bundle while an `sbxr` pod exists.
- `SHA256SUMS` in the bundle protects the copy to the node, not the supply chain: it ships in the same image. The pins above protect the inputs.

## Bundle layout

```text
/opt/sbxr/bin/containerd-shim-sbxr-v1   static Go shim; the installer links it into /usr/local/bin
/opt/sbxr/guest/       guest root: sbxr-agent, static crun in usr/bin, .krun_config.json
/opt/sbxr/lib/         libkrun.so.1, libkrunfw.so.5, and their libraries other than glibc (libgcc_s)
/opt/sbxr/share/       license texts
/opt/sbxr/NOTICE       versions, sources, and changes
/opt/sbxr/SHA256SUMS   checksums of every file; the installer checks them while it stages a copy
```

- The libraries use `$ORIGIN` to find their bundled dependencies.
- After each install, the installer runs `containerd-shim-sbxr-v1 check` on the host. The shim loads `libkrun` and `libkrunfw` from the `lib` directory of its own bundle, as its VM process does, and exits.
