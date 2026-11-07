mod config;
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
    let filter = tracing_subscriber::EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("unroxy=info"));
    tracing_subscriber::fmt().with_env_filter(filter).init();

    let handler = config::build().await;
    let listener = TcpListener::bind(("0.0.0.0", config::DEFAULT_PORT)).await?;
    tracing::info!("unroxy running on :{}", config::DEFAULT_PORT);
    Arc::new(Server::new(handler)).serve(listener).await?;
    Ok(())
}
