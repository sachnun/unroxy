use std::sync::Arc;

use futures_util::future::BoxFuture;
use tokio::net::TcpListener;
use unroxy_proxy::{NoForwardFactory, Provider, ProviderConfig, Server};
use unroxy_psiphon::serverlist::{DEFAULT_CACHE_PATH, FETCH_TIMEOUT, HttpFetch};

struct ReqwestFetch;

impl HttpFetch for ReqwestFetch {
    fn get<'a>(&'a self, url: &'a str) -> BoxFuture<'a, Result<Vec<u8>, String>> {
        Box::pin(async move {
            let client = reqwest::Client::builder()
                .timeout(FETCH_TIMEOUT)
                .build()
                .map_err(|err| err.to_string())?;
            let response = client
                .get(url)
                .send()
                .await
                .map_err(|err| err.to_string())?;
            if response.status() != 200 {
                return Err(format!("status {}", response.status()));
            }
            let body = response.bytes().await.map_err(|err| err.to_string())?;
            Ok(body.to_vec())
        })
    }
}

#[tokio::main]
async fn main() {
    let port = std::env::var("PORT")
        .ok()
        .and_then(|value| value.parse::<u16>().ok())
        .unwrap_or(8080);

    let entries = unroxy_psiphon::serverlist::load(&ReqwestFetch, DEFAULT_CACHE_PATH).await;

    let config = ProviderConfig {
        data_dir_root: std::env::var("DATA_DIR")
            .unwrap_or_else(|_| "/tmp/unroxy-psiphon".to_string()),
        ..Default::default()
    };
    let mut provider = Provider::new(Arc::new(NoForwardFactory), config);
    let regions = provider.start(&entries).await;
    if regions == 0 {
        eprintln!("unroxy: no usable region");
    }
    let handler = provider.handler();

    let listener = match TcpListener::bind(("0.0.0.0", port)).await {
        Ok(listener) => listener,
        Err(err) => {
            eprintln!("unroxy: bind {port} failed: {err}");
            return;
        }
    };
    println!("unroxy listening on 0.0.0.0:{port}");
    let _ = Arc::new(Server::new(handler)).serve(listener).await;
}
