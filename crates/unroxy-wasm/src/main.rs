#[cfg(not(target_family = "wasm"))]
fn main() {
    eprintln!("unroxy-wasm targets wasm32-wasip2");
}

#[cfg(target_family = "wasm")]
fn main() {
    use std::sync::Arc;

    use futures_util::future::BoxFuture;
    use unroxy_proxy::{NoForwardFactory, Provider, ProviderConfig, Server};
    use unroxy_psiphon::serverlist::{self, HttpFetch};

    struct WstdFetch;

    impl HttpFetch for WstdFetch {
        fn get<'a>(&'a self, url: &'a str) -> BoxFuture<'a, Result<Vec<u8>, String>> {
            Box::pin(async move {
                let client = wstd::http::Client::new();
                let request = wstd::http::Request::builder()
                    .method(wstd::http::Method::GET)
                    .uri(url)
                    .body(wstd::http::Body::empty())
                    .map_err(|err| err.to_string())?;
                let mut response = client.send(request).await.map_err(|err| err.to_string())?;
                if !response.status().is_success() {
                    return Err(format!("status {}", response.status()));
                }
                let body = response
                    .body_mut()
                    .bytes_contents()
                    .await
                    .map_err(|err| err.to_string())?;
                Ok(body.to_vec())
            })
        }
    }

    let port = std::env::var("PORT")
        .ok()
        .and_then(|value| value.parse::<u16>().ok())
        .unwrap_or(8080);
    let data_dir = std::env::var("DATA_DIR").unwrap_or_else(|_| ".".to_string());

    let raw_entries = wstd::runtime::block_on(async {
        serverlist::load(&WstdFetch, serverlist::DEFAULT_CACHE_PATH).await
    });

    let runtime = match tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
    {
        Ok(runtime) => runtime,
        Err(err) => {
            eprintln!("unroxy: runtime: {err}");
            return;
        }
    };

    runtime.block_on(async move {
        unroxy_net::init();
        let config = ProviderConfig {
            data_dir_root: data_dir,
            ..Default::default()
        };
        let mut provider = Provider::new(Arc::new(NoForwardFactory), config);
        let regions = provider.start(&raw_entries).await;
        if regions == 0 {
            eprintln!("unroxy: no usable region");
        }
        let handler = provider.handler();

        let listener = match unroxy_net::TcpListener::bind(("0.0.0.0", port)).await {
            Ok(listener) => listener,
            Err(err) => {
                eprintln!("unroxy: bind {port} failed: {err}");
                return;
            }
        };
        println!("unroxy listening on 0.0.0.0:{port}");
        let _ = Arc::new(Server::new(handler)).serve(listener).await;
    });
}
