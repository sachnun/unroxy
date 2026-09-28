# unroxy

Rotating proxy over Psiphon tunnels. One region per pool, one exit per request.

## Build

```bash
cargo build --release
```

No Go toolchain. The Psiphon tunnel core is reimplemented natively in Rust
(`crates/psiphon`), so the build is a plain Cargo build.

## Run

```bash
./target/release/unroxy
```

Listens on `:8080`. `GET /` prints usage and the pool listing.

| Request | Route |
|---|---|
| `curl -x http://HOST http://ipwho.is` | Forward proxy |
| `curl -x http://HOST https://ipwho.is` | `CONNECT` tunnel |
| `curl -x http://us@HOST https://ipwho.is` | Named region |
| `curl http://HOST/ipwho.is` | Rewrite proxy |
| `curl http://HOST/us/ipwho.is` | Rewrite proxy, named region |

## Test

```bash
cargo test
cargo clippy --all-targets -- -D warnings
cargo fmt --all --check
```

## Layout

| Path | Role |
|---|---|
| `crates/unroxy` | The proxy: routing, pools, providers, HTTP front end |
| `crates/psiphon` | Native Rust Psiphon client: server entries, obfuscated SSH, tunnel pool, SOCKS front end |
| `vendor/russh` | `russh` 0.63.3 with a generic SSH global-request API added, used for the Psiphon handshake |
