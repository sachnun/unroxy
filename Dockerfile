# The Go toolchain builds the Psiphon archive the Rust binary links against,
# so it has to match the version the core was pinned to. psiphon-tls reads Go
# runtime internals and aborts at init on a different release.
FROM golang:1.26 AS gobuild
WORKDIR /go
COPY crates/psiphon/go/go.mod crates/psiphon/go/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY crates/psiphon/go/ ./
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -buildmode=c-archive -o /out/libpsiphon.a \
    -tags "PSIPHON_DISABLE_INPROXY PSIPHON_DISABLE_QUIC PSIPHON_DISABLE_GQUIC" .

# BoringSSL is built by the btls crate, which needs a C toolchain and CMake.
FROM rust:1-slim AS builder
RUN apt-get update && apt-get install --no-install-recommends -y \
    build-essential cmake perl pkg-config libclang-dev git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY Cargo.toml Cargo.lock ./
COPY crates/psiphon/Cargo.toml crates/psiphon/Cargo.toml
COPY crates/unroxy/Cargo.toml crates/unroxy/Cargo.toml
RUN mkdir -p crates/psiphon/src crates/unroxy/src \
    && echo 'fn main() {}' > crates/unroxy/src/main.rs \
    && echo '' > crates/psiphon/src/lib.rs \
    && cargo build --release --offline 2>/dev/null || true
COPY crates/ crates/
COPY --from=gobuild /out/libpsiphon.a /go/lib/libpsiphon.a
ENV UNROXY_SKIP_GO=1
ENV UNROXY_PSIPHON_LIB_DIR=/go/lib
RUN --mount=type=cache,target=/app/target \
    cargo build --release -p unroxy && \
    cp target/release/unroxy /out-unroxy

FROM debian:stable-slim
RUN apt-get update && apt-get install --no-install-recommends -y ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /root/
COPY --from=builder /out-unroxy ./unroxy
EXPOSE 8080
CMD ["./unroxy"]
