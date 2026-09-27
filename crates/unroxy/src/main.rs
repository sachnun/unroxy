mod config;
mod emulation;
mod entries;
mod exit;
mod geo;
mod pool;
mod provider;
mod proxy;
mod serverlist;
mod socks;
#[cfg(test)]
mod testserver;
mod upstream;

use std::sync::Arc;

use tokio::net::TcpListener;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info")),
        )
        .init();

    let mut provider = provider::Provider::new();
    let handler = Arc::clone(&provider.handler);

    tokio::spawn(async move {
        let regions = provider.load().await;
        tracing::info!("psiphon: {regions} regions online");
    });

    let listener = TcpListener::bind(("0.0.0.0", config::DEFAULT_PORT)).await?;
    tracing::info!("unroxy running on :{}", config::DEFAULT_PORT);
    Arc::new(proxy::Server::new(handler))
        .serve(listener)
        .await?;
    Ok(())
}
