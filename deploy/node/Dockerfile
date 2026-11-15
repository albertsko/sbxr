# syntax=docker/dockerfile:1

# Node installer image: the phase 1 bundle (crun with libkrun, under /bundle) and sbxr-node.
# Build from the repository root: docker build -f deploy/node/Dockerfile -t sbxr-node .
# Bundle only: docker build -f deploy/node/Dockerfile --target bundle --output type=local,dest=out .
# Ubuntu 22.04 sets the glibc floor (2.35) for every binary in the bundle.
# Every input is pinned: base images by digest, sources by commit or SHA-256.

ARG RUST_IMAGE=rust:1.96-bookworm@sha256:a339861ae23e9abb272cea45dfafde21760d2ce6577a70f8a926153677902663

FROM ${RUST_IMAGE} AS rust

FROM ubuntu:22.04@sha256:74b12b11ffa550fba1929ad4d8d16533c34f6612728469671afba1b03b66881c AS build

ARG LIBKRUN_VERSION=v1.19.8
ARG LIBKRUN_COMMIT=c4d283c233bc0ae1993c289ec2a1b4da2242f1f8
ARG LIBKRUNFW_VERSION=v5.6.4
ARG LIBKRUNFW_COMMIT=b31ee3033103aaabdfef63e3d12a6ada873ace92
ARG LIBKRUNFW_SHA256_X86_64=25a5554137dcc11dd71246627264f67539154ee783aa8450acfa79139e802e64
ARG LIBKRUNFW_SHA256_AARCH64=c031d9b582b766b921701e2427c0feb318e63c38207796ef196b613f311de98e
ARG CRUN_VERSION=1.31
ARG CRUN_SHA256=17d04ba159fada1f1300a36b8afe869afb38bf1bbda28da052e83bb4bee20108

ENV DEBIAN_FRONTEND=noninteractive
# The assemble stage reads the crun version for the static guest build.
ENV CRUN_VERSION=$CRUN_VERSION
RUN apt-get update && apt-get install -y --no-install-recommends \
      autoconf automake build-essential ca-certificates clang curl git gperf libcap-dev \
      libclang-dev libjson-c-dev libseccomp-dev libsystemd-dev libtool patchelf \
      pkg-config python3 xz-utils \
    && rm -rf /var/lib/apt/lists/*

# The Rust toolchain from the pinned official image. rustc needs only an old glibc, so it runs here.
COPY --from=rust /usr/local/rustup /usr/local/rustup
COPY --from=rust /usr/local/cargo /usr/local/cargo
ENV RUSTUP_HOME=/usr/local/rustup CARGO_HOME=/usr/local/cargo PATH=/usr/local/cargo/bin:$PATH

WORKDIR /src
COPY deploy/node/fetch.sh /src/fetch.sh

# The guest init keeps the first of duplicate env entries, but containerd lists the image env first and the pod env after it.
# The patch makes the last entry win, so that pod env overrides image env as with runc. collect.sh records it in NOTICE.
RUN /src/fetch.sh git https://github.com/libkrun/libkrun "$LIBKRUN_VERSION" "$LIBKRUN_COMMIT" libkrun \
    && init=libkrun/src/init_blob/init/init.c \
    && test "$(grep -c 'setenv(env, env_val, 0);' "$init")" = 1 \
    && sed -i 's/setenv(env, env_val, 0);/setenv(env, env_val, 1);/' "$init" \
    && test "$(grep -c 'setenv(env, env_val, 1);' "$init")" = 2 \
    && make -C libkrun -j"$(nproc)" NET=1 \
    && make -C libkrun install NET=1 PREFIX=/usr/local LIBDIR_Linux=lib

RUN /src/fetch.sh tar "https://github.com/containers/crun/releases/download/$CRUN_VERSION/crun-$CRUN_VERSION.tar.gz" "$CRUN_SHA256" crun 1 \
    && cd crun \
    && ./configure --prefix=/usr/local --with-libkrun --disable-criu \
    && make -j"$(nproc)"

RUN case "$(uname -m)" in x86_64) sum=$LIBKRUNFW_SHA256_X86_64 ;; aarch64) sum=$LIBKRUNFW_SHA256_AARCH64 ;; esac \
    && /src/fetch.sh tar "https://github.com/libkrun/libkrunfw/releases/download/$LIBKRUNFW_VERSION/libkrunfw-$(uname -m).tgz" "$sum" fw \
    && /src/fetch.sh git https://github.com/libkrun/libkrunfw "$LIBKRUNFW_VERSION" "$LIBKRUNFW_COMMIT" libkrunfw

COPY deploy/node/collect.sh /src/collect.sh
RUN /src/collect.sh /opt/sbxr

# The installer, the phase 2 shim, and the guest agent: static Go programs.
FROM golang:1.27@sha256:ec47ea3725fea0cc88f6de94919aef19f40f12fc1ec0e2d1b3426dbf15ecca04 AS go
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /out/ ./cmd/sbxr-node ./cmd/containerd-shim-sbxr-v1 ./cmd/sbxr-agent \
    && go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Dir}}{{end}}{{end}}' \
      ./cmd/sbxr-node ./cmd/containerd-shim-sbxr-v1 ./cmd/sbxr-agent | sort -u \
      | while read -r mod dir; do mkdir -p "/out/licenses/go/$mod" && cp "$dir"/LICENSE* "$dir"/NOTICE* "/out/licenses/go/$mod/" 2>/dev/null; test -n "$(ls "/out/licenses/go/$mod")"; done

FROM build AS assemble
ARG CRUN_STATIC_SHA256_AMD64=44d493d4b00914ea2f153631ad825c7653aacfa09c373c50dfe08c61258ede9d
ARG CRUN_STATIC_SHA256_ARM64=2fd023c8a110edd31a2eb2617a3d7633d2615364938250c89cb13fffd9f0ed38
COPY --from=go /out /go-out
COPY deploy/node/assemble.sh /src/assemble.sh
RUN case "$(dpkg --print-architecture)" in amd64) sum=$CRUN_STATIC_SHA256_AMD64 ;; arm64) sum=$CRUN_STATIC_SHA256_ARM64 ;; esac \
    && CRUN_STATIC_SHA256=$sum /src/assemble.sh /opt/sbxr /go-out

FROM scratch AS bundle
COPY --from=assemble /opt/sbxr /

# busybox provides nsenter, which runs host commands in the host mount namespace.
FROM busybox:1.37@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec
COPY --from=assemble /opt/sbxr /bundle
COPY --from=go /out/sbxr-node /usr/local/bin/sbxr-node
ENTRYPOINT ["/usr/local/bin/sbxr-node"]
