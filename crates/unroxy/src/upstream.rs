use std::collections::HashMap;
use std::sync::{Arc, LazyLock, Mutex};
use std::time::{Duration, Instant};

use http::Method;
use wreq::redirect;

use crate::pool::{Candidate, ProxyPool};

pub const NOT_READY_WAIT: Duration = Duration::from_secs(30);

const POOL_MAX_IDLE: usize = 16;
const POOL_IDLE_TIMEOUT: Duration = Duration::from_secs(30);

struct CachedClient {
    generation: u64,
    client: wreq::Client,
}

static CLIENTS: LazyLock<Mutex<HashMap<Arc<str>, CachedClient>>> =
    LazyLock::new(|| Mutex::new(HashMap::new()));

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("no upstream proxies available")]
    NoUpstream,
    #[error("upstream request failed: {0}")]
    Request(#[from] wreq::Error),
}

pub struct RotatingTransport {
    pool: Arc<ProxyPool>,
}

impl RotatingTransport {
    pub fn new(pool: Arc<ProxyPool>) -> Arc<Self> {
        Arc::new(Self { pool })
    }

    fn client(&self, candidate: &Candidate) -> Result<wreq::Client, Error> {
        let generation = candidate.tunnel.generation();
        let mut clients = CLIENTS.lock().expect("client cache");
        if let Some(cached) = clients.get(&candidate.key)
            && cached.generation == generation
        {
            return Ok(cached.client.clone());
        }
        let proxy = wreq::Proxy::all(candidate.tunnel.proxy_url())?;
        let client = wreq::Client::builder()
            .emulation(crate::emulation::next())
            .proxy(proxy)
            .redirect(redirect::Policy::none())
            .pool_max_idle_per_host(POOL_MAX_IDLE)
            .pool_idle_timeout(Some(POOL_IDLE_TIMEOUT))
            .build()?;
        clients.insert(
            Arc::clone(&candidate.key),
            CachedClient {
                generation,
                client: client.clone(),
            },
        );
        Ok(client)
    }

    fn evict(&self, key: &str) {
        CLIENTS.lock().expect("client cache").remove(key);
    }

    pub async fn request(
        &self,
        request: http::Request<wreq::Body>,
    ) -> Result<(Candidate, wreq::Response), Error> {
        let target_host = request.uri().host().unwrap_or_default().to_lowercase();
        let candidate = self.ready_candidate(&target_host).await?;

        let method = request.method().clone();
        let uri = request.uri().to_string();
        let headers = request.headers().clone();
        let retryable = matches!(method, Method::GET | Method::HEAD);

        let client = self.client(&candidate)?;
        let first = self
            .send(
                &client,
                method.clone(),
                &uri,
                headers.clone(),
                request.into_body(),
                &target_host,
            )
            .await;

        match first {
            Ok(response) => Ok((candidate, response)),
            Err(err) if retryable => {
                tracing::debug!("upstream failed ({err}), retrying on a fresh connection");
                self.evict(&candidate.key);
                let client = self.client(&candidate)?;
                self.send(
                    &client,
                    method,
                    &uri,
                    headers,
                    wreq::Body::from(""),
                    &target_host,
                )
                .await
                .map(|response| (candidate, response))
            }
            Err(err) => Err(err),
        }
    }

    async fn send(
        &self,
        client: &wreq::Client,
        method: Method,
        uri: &str,
        headers: http::HeaderMap,
        body: wreq::Body,
        target_host: &str,
    ) -> Result<wreq::Response, Error> {
        client
            .request(method, uri.to_string())
            .headers(headers)
            .group(wreq::Group::new(target_host.to_string()))
            .body(body)
            .send()
            .await
            .map_err(Error::Request)
    }

    pub async fn pick(&self, target_host: &str) -> Result<Candidate, Error> {
        self.ready_candidate(target_host).await
    }

    async fn ready_candidate(&self, target_host: &str) -> Result<Candidate, Error> {
        let deadline = Instant::now() + NOT_READY_WAIT;
        loop {
            let candidates = self.pool.candidates(target_host);
            if candidates.is_empty() {
                return Err(Error::NoUpstream);
            }
            if let Some(ready) = candidates.iter().find(|candidate| candidate.is_ready()) {
                return Ok(ready.clone());
            }
            if Instant::now() >= deadline {
                return Ok(candidates[0].clone());
            }
            tokio::time::sleep(Duration::from_millis(500)).await;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::pool::Proxy;

    fn pool_with(keys: &[&str]) -> Arc<ProxyPool> {
        ProxyPool::new(
            keys.iter()
                .map(|key| Proxy {
                    key: (*key).into(),
                    tunnel: unroxy_psiphon::Tunnel::stub(key, 1),
                    priority: 0,
                })
                .collect(),
        )
    }

    #[tokio::test]
    async fn empty_pool_fails_fast() {
        let transport = RotatingTransport::new(pool_with(&[]));
        let request = http::Request::builder()
            .uri("http://example.com/")
            .body(wreq::Body::from(""))
            .unwrap();
        assert!(matches!(
            transport.request(request).await,
            Err(Error::NoUpstream)
        ));
    }
}
