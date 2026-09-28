FROM rust:1-slim AS builder
RUN apt-get update && apt-get install --no-install-recommends -y \
    build-essential cmake perl pkg-config libclang-dev git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY Cargo.toml Cargo.lock ./
COPY crates/psiphon/Cargo.toml crates/psiphon/Cargo.toml
COPY crates/unroxy/Cargo.toml crates/unroxy/Cargo.toml
COPY vendor/russh/Cargo.toml vendor/russh/Cargo.toml
RUN mkdir -p crates/psiphon/src crates/unroxy/src vendor/russh/src \
    && echo 'fn main() {}' > crates/unroxy/src/main.rs \
    && echo '' > crates/psiphon/src/lib.rs \
    && echo '' > vendor/russh/src/lib.rs \
    && cargo build --release --locked
COPY crates/ crates/
COPY vendor/ vendor/
RUN cargo build --release --locked -p unroxy && cp target/release/unroxy /out-unroxy

FROM debian:stable-slim
RUN apt-get update && apt-get install --no-install-recommends -y ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /root/
COPY --from=builder /out-unroxy ./unroxy
EXPOSE 8080
CMD ["./unroxy"]
