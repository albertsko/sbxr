#!/usr/bin/env bash
# A4: installs one distribution, prints its rendered containerd config, and checks that a drop-in adds a runtime handler.
# Usage: dropin.sh rke2|k3s|k0s <version>
set -euo pipefail

distro=$1
version=$2

case $distro in
  rke2)
    curl -sfL https://get.rke2.io | sudo INSTALL_RKE2_VERSION="$version" sh -
    sudo systemctl enable --now rke2-server
    unit=rke2-server
    dir=/var/lib/rancher/rke2/agent/etc/containerd
    sock=/run/k3s/containerd/containerd.sock
    kubectl() { sudo /var/lib/rancher/rke2/bin/kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml "$@"; }
    ;;
  k3s)
    curl -sfL https://get.k3s.io | sudo INSTALL_K3S_VERSION="$version" sh -
    unit=k3s
    dir=/var/lib/rancher/k3s/agent/etc/containerd
    sock=/run/k3s/containerd/containerd.sock
    kubectl() { sudo k3s kubectl "$@"; }
    ;;
  k0s)
    curl -sSLf https://get.k0s.sh | sudo K0S_VERSION="$version" sh
    sudo k0s install controller --single
    sudo k0s start
    unit=k0scontroller
    dir=/etc/k0s
    sock=/run/k0s/containerd.sock
    kubectl() { sudo k0s kubectl "$@"; }
    ;;
esac

crictl() { sudo /usr/local/bin/crictl --runtime-endpoint "unix://$sock" "$@"; }
curl -fsSL https://github.com/kubernetes-sigs/cri-tools/releases/download/v1.37.0/crictl-v1.37.0-linux-amd64.tar.gz | sudo tar xz -C /usr/local/bin

for _ in $(seq 120); do
  if kubectl get node 2>/dev/null | grep -q ' Ready'; then
    break
  fi
  sleep 2
done
kubectl get node -o wide
crictl version

echo "::group::containerd config directory"
sudo find "$dir" -maxdepth 2 | sort
echo "::endgroup::"
for f in "$dir"/config.toml "$dir"/config-v3.toml "$dir"/containerd.toml /run/k0s/containerd-cri.toml; do
  if sudo test -f "$f"; then
    echo "::group::$f"
    sudo cat "$f"
    echo "::endgroup::"
    echo "imports in $f: $(sudo grep -n '^imports' "$f" || echo none)"
  fi
done

case $distro in
  k0s) dropin=/etc/k0s/containerd.d ;;
  *) dropin=$dir/config-v3.toml.d ;;
esac
sudo mkdir -p "$dropin"
sudo tee "$dropin/sbxr.toml" >/dev/null <<'TOML'
version = 3
[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.sbxrprobe]
  runtime_type = "io.containerd.runc.v2"
  [plugins."io.containerd.cri.v1.runtime".containerd.runtimes.sbxrprobe.options]
    BinaryName = "/usr/bin/false"
TOML
echo "containerd before restart: $(ps -o pid=,lstart= -C containerd || true)"
sudo systemctl restart "$unit"
for _ in $(seq 90); do
  if crictl info 2>/dev/null | grep -q sbxrprobe; then
    break
  fi
  sleep 2
done
echo "containerd after restart: $(ps -o pid=,lstart= -C containerd || true)"
sudo journalctl -u "$unit" --no-pager | grep -i -E "containerd (is now running|.*exited|.*already)|Running containerd|killing" | tail -8 || true

for f in "$dir"/config.toml "$dir"/containerd.toml /run/k0s/containerd-cri.toml; do
  if sudo test -f "$f"; then
    echo "imports in $f after drop-in: $(sudo grep -n '^imports' "$f" || echo none)"
  fi
done
sleep 20
echo "node runtimeHandlers: $(kubectl get node -o jsonpath='{.items[0].status.runtimeHandlers[*].name}')"
if crictl info | grep -q sbxrprobe; then
  echo "RESULT $distro $version: drop-in $dropin imported"
else
  echo "RESULT $distro $version: drop-in $dropin NOT imported"
  exit 1
fi
