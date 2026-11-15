#!/usr/bin/env bash
# A2 local check: runs smoke.sh inside the userland of each given image, through chroot, as root.
# Usage: chroot-smoke.sh <bundle-dir> <image>...
# Used in the Lima VM, where only rootless nerdctl exists.
set -euo pipefail

bundle=$(realpath "$1")
shift
here=$(dirname "$(realpath "$0")")
work=/tmp/nb-chroot
sudo rm -rf "$work"
mkdir -p "$work"

export_rootfs() {
  local image=$1 dest=$2
  nerdctl pull -q "$image" >/dev/null
  local id
  id=$(nerdctl create --entrypoint /bin/true "$image")
  sudo mkdir -p "$dest"
  nerdctl export "$id" | sudo tar x -C "$dest"
  nerdctl rm "$id" >/dev/null
}

export_rootfs busybox:1.37 "$work/busybox"

for image in "$@"; do
  root=$work/$(echo "$image" | tr ':/' '__')
  export_rootfs "$image" "$root"
  sudo mkdir -p "$root/opt/sbxr" "$root/rootfs"
  sudo cp -a "$bundle/." "$root/opt/sbxr/"
  sudo cp -a "$work/busybox/." "$root/rootfs/"
  sudo cp "$here/../../deploy/bundle/smoke.sh" "$root/smoke.sh"

  set +e
  sudo unshare -m --propagation private sh -c "
    mount --bind '$root' '$root' &&
    mount --rbind /proc '$root/proc' && mount --rbind /sys '$root/sys' && mount --rbind /dev '$root/dev' &&
    chroot '$root' sh -c 'head -1 /etc/os-release; ldd --version | head -1; bash /smoke.sh /opt/sbxr /rootfs'"
  code=$?
  set -e
  echo "RESULT $image: exit $code"
done
