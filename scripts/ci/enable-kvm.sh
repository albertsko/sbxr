#!/usr/bin/env bash
# Lets every user on a hosted Linux runner open /dev/kvm.
# Usage: scripts/ci/enable-kvm.sh
set -euo pipefail

echo 'KERNEL=="kvm", GROUP="kvm", MODE="0666", OPTIONS+="static_node=kvm"' | sudo tee /etc/udev/rules.d/99-kvm4all.rules
sudo udevadm control --reload-rules
sudo udevadm trigger --name-match=kvm
ls -l /dev/kvm
