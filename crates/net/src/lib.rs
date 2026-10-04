#[cfg(target_family = "wasm")]
mod wasi;
#[cfg(target_family = "wasm")]
pub use wasi::{TcpListener, TcpStream, init};

#[cfg(not(target_family = "wasm"))]
mod native;
#[cfg(not(target_family = "wasm"))]
pub use native::{TcpListener, TcpStream, init};
