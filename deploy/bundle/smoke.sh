#!/usr/bin/env bash
# Runs one container in a VM through the bundle's krun link, with no containerd.
# Usage: smoke.sh <bundle-dir> <rootfs-dir>
# Needs root and /dev/kvm. Expects "hi" on stdout and exit code 3.
set -euo pipefail

bundle=$1
rootfs=$2

"$bundle/bin/krun" --version | grep -q '+LIBKRUN'

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
ln -s "$(realpath "$rootfs")" "$work/rootfs"

cd "$work"
"$bundle/bin/krun" spec
sed -i \
  -e 's/"terminal": true/"terminal": false/' \
  -e 's|^\(\s*\)"sh"$|\1"/bin/sh", "-c", "echo hi; exit 3"|' \
  config.json

set +e
out=$("$bundle/bin/krun" run smoke 2>&1)
code=$?
set -e

echo "output: $out"
echo "exit: $code"
[[ $out == *hi* && $code == 3 ]]
