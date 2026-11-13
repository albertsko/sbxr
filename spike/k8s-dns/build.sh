#!/usr/bin/env bash
# Builds crun with libkrun inside kindest/node and copies the results to /out.
set -euxo pipefail

export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -y -q --no-install-recommends build-essential git curl ca-certificates pkg-config \
  libclang-dev libjson-c-dev libcap-dev libseccomp-dev gperf python3 autoconf automake libtool

curl -sSf https://sh.rustup.rs | sh -s -- -y --profile minimal
# shellcheck disable=SC1091
. "$HOME/.cargo/env"

mkdir -p /tmp/fw
curl -fsSL "https://github.com/containers/libkrunfw/releases/download/$LIBKRUNFW_VERSION/libkrunfw-x86_64.tgz" | tar -xz -C /tmp/fw
find /tmp/fw
find /tmp/fw -name 'libkrunfw.so*' -exec cp -a {} /usr/local/lib/ \;
ldconfig

git clone -q --depth 1 --branch "$LIBKRUN_VERSION" https://github.com/containers/libkrun.git /tmp/libkrun
make -C /tmp/libkrun -j"$(nproc)"
make -C /tmp/libkrun install PREFIX=/usr/local LIBDIR_Linux=lib
ldconfig

if curl -fsSL "https://github.com/containers/crun/releases/download/$CRUN_VERSION/crun-$CRUN_VERSION.tar.gz" | tar -xz -C /tmp; then
  cd "/tmp/crun-$CRUN_VERSION"
else
  git clone -q --depth 1 --recursive --branch "$CRUN_VERSION" https://github.com/containers/crun.git /tmp/crun-git
  cd /tmp/crun-git
  ./autogen.sh
fi
PKG_CONFIG_PATH=/usr/local/lib/pkgconfig ./configure --with-libkrun --disable-systemd --prefix=/usr/local
make -j"$(nproc)"
make install

mkdir -p /out/bin /out/lib
cp -a /usr/local/bin/crun /out/bin/
cp -a /usr/local/lib/libkrun*.so* /out/lib/
cp -a /usr/lib/x86_64-linux-gnu/libjson-c.so.5* /out/lib/
ldd /usr/local/bin/crun
/usr/local/bin/crun --version
ls -la /out/bin /out/lib
