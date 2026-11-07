mod config;
mod dial;
mod emulation;
mod forward;
mod geo;
#[cfg(test)]
mod memcheck;
mod openvpn;
mod proxy;
mod ssh;
#[cfg(test)]
mod testserver;
mod upstream;
mod vpn;

use std::sync::Arc;

use tokio::net::TcpListener;

use crate::proxy::Server;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt().init();

    let handler = config::build().await;
    let listener = TcpListener::bind(("0.0.0.0", config::DEFAULT_PORT)).await?;
    tracing::info!("unroxy running on :{}", config::DEFAULT_PORT);
    Arc::new(Server::new(handler)).serve(listener).await?;
    Ok(())
}
