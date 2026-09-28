# vendored russh

`russh` 0.63.3, unmodified except for one addition: a generic SSH global
request API.

`russh` exposes `tcpip_forward`, `keepalive@openssh.com`, and a few other
fixed global requests, but no way to send an arbitrary global request and read
the reply. The Psiphon tunnel requires exactly that: after SSH authentication,
the client sends a `psiphon-handshake` global request; the server only permits
TCP port forwarding once that request has completed.

The change adds:

| Item | Location |
|---|---|
| `client::Handle::send_request(name, want_reply, data)` | `src/client/mod.rs` |
| `Msg::GlobalRequest { reply, name, want_reply, data }` | `src/client/mod.rs` |
| `GlobalRequestResponse::Global(oneshot::Sender<Option<Vec<u8>>>)` | `src/session.rs` |
| request writer | `src/client/session.rs` |
| reply reader for `REQUEST_SUCCESS` / `REQUEST_FAILURE` | `src/client/encrypted.rs` |

Applied via `[patch.crates-io]` in the workspace `Cargo.toml`.
