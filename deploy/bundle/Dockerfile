# syntax=docker/dockerfile:1

# Phase 1 node bundle: crun with libkrun, plus every library the nodes may lack.
# Build: docker build -f deploy/bundle/Dockerfile --output type=local,dest=out deploy/bundle
# Install: copy out/ to PREFIX on the node. crun finds its libraries only there.
# Ubuntu 22.04 sets the glibc floor (2.35) for every binary in the bundle.

FROM ubuntu:22.04 AS build

ARG LIBKRUN_VERSION=v1.19.8
ARG LIBKRUNFW_VERSION=v5.6.4
ARG CRUN_VERSION=1.31
ARG PREFIX=/opt/sbxr

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      autoconf automake build-essential ca-certificates clang curl git gperf libcap-dev \
      libclang-dev libjson-c-dev libseccomp-dev libsystemd-dev libtool patchelf \
      pkg-config python3 xz-utils \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://sh.rustup.rs | sh -s -- -y --profile minimal
ENV PATH=/root/.cargo/bin:$PATH

WORKDIR /src
RUN git clone -q --depth 1 -b "$LIBKRUN_VERSION" https://github.com/libkrun/libkrun \
    && make -C libkrun -j"$(nproc)" NET=1 \
    && make -C libkrun install NET=1 PREFIX=/usr/local LIBDIR_Linux=lib

RUN mkdir crun \
    && curl -fsSL "https://github.com/containers/crun/releases/download/$CRUN_VERSION/crun-$CRUN_VERSION.tar.gz" \
      | tar xz -C crun --strip-components=1 \
    && cd crun \
    && ./configure --prefix=/usr/local --with-libkrun --disable-criu \
    && make -j"$(nproc)"

RUN mkdir fw \
    && curl -fsSL "https://github.com/libkrun/libkrunfw/releases/download/$LIBKRUNFW_VERSION/libkrunfw-$(uname -m).tgz" \
      | tar xz -C fw

COPY collect.sh /src/collect.sh
RUN /src/collect.sh "$PREFIX"

FROM scratch
ARG PREFIX=/opt/sbxr
COPY --from=build $PREFIX /
