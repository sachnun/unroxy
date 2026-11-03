FROM rust:1-slim AS builder
RUN apt-get update && apt-get install --no-install-recommends -y \
    build-essential cmake perl pkg-config libclang-dev git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY Cargo.toml Cargo.lock ./
RUN mkdir -p src \
    && echo 'fn main() {}' > src/main.rs \
    && cargo build --release --locked
COPY src/ src/
RUN find src -name '*.rs' -exec touch {} + \
    && cargo build --release --locked \
    && cp target/release/unroxy /out-unroxy \
    && test "$(stat -c%s /out-unroxy)" -gt 1000000

FROM debian:stable-slim
RUN apt-get update && apt-get install --no-install-recommends -y ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /root/
COPY --from=builder /out-unroxy ./unroxy
EXPOSE 8080
CMD ["./unroxy"]
