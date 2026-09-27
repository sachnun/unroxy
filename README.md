# unroxy

Rotating proxy over Psiphon tunnels. One region per pool, one exit per request.

## Build

```bash
cargo build --release
```

The build needs Go 1.26. The Psiphon tunnel core is compiled from a pinned
release and its TLS fork checks Go runtime layout at init, so a different
toolchain aborts at startup. Set `UNROXY_GO_TOOLCHAIN` to override the default.

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
| `crates/psiphon` | Rust wrapper over the Go tunnel core, plus the cgo source in `go/` |
