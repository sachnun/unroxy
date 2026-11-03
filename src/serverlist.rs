use crate::psiphon::serverlist::{DEFAULT_CACHE_PATH, FETCH_TIMEOUT, HttpFetch};
use futures_util::future::BoxFuture;

struct WreqFetch;

impl HttpFetch for WreqFetch {
    fn get<'a>(&'a self, url: &'a str) -> BoxFuture<'a, Result<Vec<u8>, String>> {
        Box::pin(async move {
            let client = wreq::Client::builder()
                .emulation(crate::emulation::next())
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

pub async fn load() -> String {
    crate::psiphon::serverlist::load(&WreqFetch, DEFAULT_CACHE_PATH).await
}
