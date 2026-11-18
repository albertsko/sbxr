#!/usr/bin/env bash
# Installs uidmap and gives the runner user a subordinate id range, which sbxr needs for its user namespace.
# Usage: scripts/ci/enable-subids.sh
set -euo pipefail

sudo apt-get install -y -q uidmap
if ! grep -q "^$USER:" /etc/subuid; then
	sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 "$USER"
fi
grep "^$USER:" /etc/subuid /etc/subgid
