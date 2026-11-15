#!/usr/bin/env bash
# Adds the phase 2 runtime to the bundle at <prefix>: the containerd shim, the guest root with the agent and a static crun,
# and the licenses of the Go modules. Then it writes SHA256SUMS for the whole bundle.
# Usage: assemble.sh <prefix> <go-out>
# Environment: CRUN_VERSION, CRUN_STATIC_SHA256 (of crun-<version>-linux-<arch>-disable-systemd).
set -euo pipefail

prefix=$1
out=$2
guest=$prefix/guest

install -m 755 "$out/containerd-shim-sbxr-v1" "$prefix/bin/containerd-shim-sbxr-v1"

# The guest root is shared read-only with every pod VM. The libkrun init starts the agent as PID 1.
mkdir -p "$guest"/{dev,proc,sys,run,tmp,etc,usr/bin}
install -m 755 "$out/sbxr-agent" "$guest/sbxr-agent"
arch=$(dpkg --print-architecture)
/src/fetch.sh file "https://github.com/containers/crun/releases/download/$CRUN_VERSION/crun-$CRUN_VERSION-linux-$arch-disable-systemd" \
	"$CRUN_STATIC_SHA256" "$guest/usr/bin/crun"
chmod 755 "$guest/usr/bin/crun"
cat >"$guest/.krun_config.json" <<'JSON'
{"Cmd":["/sbxr-agent"],"Env":["KRUN_INIT_PID1=1","PATH=/usr/bin:/bin"],"WorkingDir":"/"}
JSON

cp -a "$out/licenses/." "$prefix/share/licenses/"
{
	echo
	echo "Guest crun $CRUN_VERSION: the static release build crun-$CRUN_VERSION-linux-$arch-disable-systemd."
	echo "containerd-shim-sbxr-v1, sbxr-agent, and sbxr-node are Go programs. share/licenses/go holds the licenses of the Go modules they include."
} >>"$prefix/NOTICE"

# The installer checks every file against this list while it copies the bundle to the node.
sums=$(cd "$prefix" && find . -type f -exec sha256sum {} + | sort -k2)
echo "$sums" >"$prefix/SHA256SUMS"
ls -lA "$prefix/bin" "$guest" "$guest/usr/bin"
du -sh "$prefix"
