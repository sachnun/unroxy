//! Upstream selection and request routing, ported from
//! `internal/core/transport.go`.

use std::collections::HashMap;
use std::sync::{Arc, LazyLock, Mutex};
use std::time::{Duration, Instant};

use wreq::redirect;

use crate::pool::{Candidate, ProxyPool};

pub const NOT_READY_WAIT: Duration = Duration::from_secs(30);

/// One client per tunnel, shared by every transport. A region is dialed both by
/// its own transport and by the primaries, and a client carries a TLS context,
/// so keeping one per transport would duplicate the heavy part.
static CLIENTS: LazyLock<Mutex<HashMap<Arc<str>, wreq::Client>>> =
    LazyLock::new(|| Mutex::new(HashMap::new()));

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("no upstream proxies available")]
    NoUpstream,
    #[error("upstream request failed: {0}")]
    Request(#[from] wreq::Error),
}

/// Sends requests through the proxy pool, one upstream attempt per request.
pub struct RotatingTransport {
    pool: Arc<ProxyPool>,
}

impl RotatingTransport {
    pub fn new(pool: Arc<ProxyPool>) -> Arc<Self> {
        Arc::new(Self { pool })
    }

    fn client(&self, candidate: &Candidate) -> Result<wreq::Client, Error> {
        let mut clients = CLIENTS.lock().expect("client cache");
        if let Some(client) = clients.get(&candidate.key) {
            return Ok(client.clone());
        }
        let proxy = wreq::Proxy::all(candidate.tunnel.proxy_url())?;
        // No connection reuse. A tunnel can be retired at any moment and its
        // pooled connections die with it; a reused dead connection shows up as
        // a stalled request. The Go transport sets DisableKeepAlives for the
        // same reason, so this is the source behaviour, not a workaround.
        let client = wreq::Client::builder()
            .emulation(crate::emulation::next())
            .proxy(proxy)
            .redirect(redirect::Policy::none())
            .pool_max_idle_per_host(0)
            .pool_idle_timeout(None)
            .build()?;
        clients.insert(Arc::clone(&candidate.key), client.clone());
        Ok(client)
    }

    /// Runs one request against one upstream, returning the response and the
    /// candidate that carried it. The caller owns both.
    pub async fn request(
        &self,
        request: http::Request<wreq::Body>,
    ) -> Result<(Candidate, wreq::Response), Error> {
        let target_host = request.uri().host().unwrap_or_default().to_lowercase();

        let candidate = self.ready_candidate(&target_host).await?;
        let client = self.client(&candidate)?;

        let (parts, body) = request.into_parts();
        let builder = client
            .request(parts.method, parts.uri.to_string())
            .headers(parts.headers)
            .group(wreq::Group::new(target_host.clone()))
            .body(body);

        match builder.send().await {
            Ok(response) => Ok((candidate, response)),
            Err(err) => Err(Error::Request(err)),
        }
    }

    /// Picks a candidate, waiting for a ready tunnel the way the Go transport
    /// does.
    pub async fn pick(&self, target_host: &str) -> Result<Candidate, Error> {
        self.ready_candidate(target_host).await
    }

    /// Tunnel failures are not recorded: a tunnel that cannot dial is replaced
    /// by the controller, and demoting it would keep the pool pinned to the
    /// last working exit.
    async fn ready_candidate(&self, target_host: &str) -> Result<Candidate, Error> {
        let deadline = Instant::now() + NOT_READY_WAIT;
        loop {
            let candidates = self.pool.candidates(target_host);
            let Some(first) = candidates.first() else {
                return Err(Error::NoUpstream);
            };
            if candidates.iter().any(|candidate| candidate.is_ready()) || Instant::now() >= deadline
            {
                return Ok(first.clone());
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
