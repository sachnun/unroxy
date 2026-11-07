#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("account: {0}")]
    Account(String),
    #[error("tls: {0}")]
    Tls(String),
    #[error("control channel: {0}")]
    Control(String),
    #[error("data channel: {0}")]
    Data(String),
    #[error("handshake timed out")]
    Timeout,
    #[error("authentication rejected")]
    Auth,
    #[error("transport: {0}")]
    Transport(String),
    #[error("io: {0}")]
    Io(#[from] std::io::Error),
}
