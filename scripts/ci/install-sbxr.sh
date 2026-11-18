#!/usr/bin/env bash
# Installs an sbxr binary as /usr/local/bin/sbxr with its AppArmor profile.
# The hosted Ubuntu runner restricts user namespaces with AppArmor, and the profile covers only the installed path.
# Usage: scripts/ci/install-sbxr.sh <sbxr-binary>
set -euo pipefail

sudo install -m 755 "$1" /usr/local/bin/sbxr
sbxr doctor apparmor-profile | sudo tee /etc/apparmor.d/sbxr >/dev/null
sudo apparmor_parser -r /etc/apparmor.d/sbxr
