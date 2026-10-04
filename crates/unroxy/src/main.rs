mod config;
mod emulation;
mod forward;
mod geo;
#[cfg(test)]
mod memcheck;
mod serverlist;
#[cfg(test)]
mod testserver;
mod upstream;

use std::sync::Arc;

use tokio::net::TcpListener;
use unroxy_proxy::{Provider, Server};

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt().init();

    let factory = Arc::new(forward::WreqFactory);
    let mut provider = Provider::new(factory, config::provider_config());
    let handler = provider.handler();

    tokio::spawn(async move {
        let raw = serverlist::load().await;
        let regions = provider.start(&raw).await;
        tracing::info!("psiphon: {regions} regions online");
    });

    let listener = TcpListener::bind(("0.0.0.0", config::DEFAULT_PORT)).await?;
    tracing::info!("unroxy running on :{}", config::DEFAULT_PORT);
    Arc::new(Server::new(handler)).serve(listener).await?;
    Ok(())
}
