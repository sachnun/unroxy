# Vendored russh-util

`russh-util` 0.52.0, patched for WASIX/WASI.

Upstream gates its runtime on `target_arch = "wasm32"`, which is true for
`wasm32-wasmer-wasi` too, and then uses `wasm_bindgen_futures::spawn_local`
and `chrono` via a JS host. A WASIX module has no JS host, so the tunnel's
SSH channel tasks never run.

The patch narrows that branch to the browser target only (`target_os =
"unknown"`), so WASIX/WASI uses the tokio path (`tokio::spawn` and
`std::time::Instant`) like every other non-browser target.

Applied via `[patch.crates-io]` in the workspace `Cargo.toml`.
