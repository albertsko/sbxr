#!/usr/bin/env bash
# Usage: build.sh OUT_DIR. Builds the spike, the guest helper for the VM, and the helper for the host.
set -euo pipefail

out=$1
here=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$out"
cd "$here"

guest_arch=$(go env GOARCH)
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$out/gvnet" .
CGO_ENABLED=0 GOOS=linux GOARCH="$guest_arch" go build -trimpath -ldflags='-s -w' -o "$out/guest-linux" ./guest
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$out/guest-host" ./guest

if [[ $(uname -s) == Darwin ]]; then
  codesign --force --sign - --entitlements "$here/entitlements.plist" "$out/gvnet"
fi

CGO_ENABLED=0 go build -tags baseline -trimpath -ldflags='-s -w' -o "$out/baseline" .
if [[ $(uname -s) == Darwin ]]; then
  codesign --force --sign - --entitlements "$here/entitlements.plist" "$out/baseline"
fi
