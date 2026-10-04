use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, RwLock};
use std::time::{Duration, Instant};

use unroxy_psiphon::Tunnel;

const PICK_WAIT: Duration = Duration::from_secs(30);

#[derive(Clone)]
pub struct PoolProxy {
    pub key: Arc<str>,
    pub tunnel: Arc<Tunnel>,
    pub priority: usize,
}

#[derive(Clone)]
pub struct Candidate {
    pub key: Arc<str>,
    pub tunnel: Arc<Tunnel>,
    pub priority: usize,
}

impl Candidate {
    pub fn is_ready(&self) -> bool {
        self.tunnel.is_ready()
    }
}

struct Inner {
    proxies: Vec<PoolProxy>,
}

pub struct ProxyPool {
    inner: RwLock<Inner>,
    rotation: AtomicU64,
}

impl ProxyPool {
    pub fn new(proxies: Vec<PoolProxy>) -> Arc<Self> {
        Arc::new(Self {
            inner: RwLock::new(Inner { proxies }),
            rotation: AtomicU64::new(0),
        })
    }

    pub fn candidates(&self, _target_host: &str) -> Vec<Candidate> {
        let inner = self.inner.read().expect("pool lock");
        if inner.proxies.is_empty() {
            return Vec::new();
        }

        let mut ready: Vec<Candidate> = inner
            .proxies
            .iter()
            .map(|proxy| Candidate {
                key: proxy.key.clone(),
                tunnel: Arc::clone(&proxy.tunnel),
                priority: proxy.priority,
            })
            .collect();
        ready.sort_by_key(|candidate| candidate.priority);

        if ready.len() > 1 {
            let offset =
                (self.rotation.fetch_add(1, Ordering::Relaxed) % ready.len() as u64) as usize;
            ready.rotate_left(offset);
        }
        ready
    }

    pub async fn pick(&self, target_host: &str) -> Option<Candidate> {
        let deadline = Instant::now() + PICK_WAIT;
        loop {
            let candidates = self.candidates(target_host);
            if candidates.is_empty() {
                return None;
            }
            if let Some(ready) = candidates.iter().find(|candidate| candidate.is_ready()) {
                return Some(ready.clone());
            }
            if Instant::now() >= deadline {
                return Some(candidates[0].clone());
            }
            tokio::time::sleep(Duration::from_millis(500)).await;
        }
    }

    pub fn proxies(&self) -> Vec<PoolProxy> {
        self.inner.read().expect("pool lock").proxies.clone()
    }

    pub fn replace(&self, proxies: Vec<PoolProxy>) {
        let mut inner = self.inner.write().expect("pool lock");
        inner.proxies = proxies;
    }

    pub fn count(&self) -> usize {
        self.inner.read().expect("pool lock").proxies.len()
    }

    pub fn tunnel_count(&self) -> usize {
        self.inner
            .read()
            .expect("pool lock")
            .proxies
            .iter()
            .map(|proxy| proxy.tunnel.target_pool())
            .sum()
    }

    pub fn usable_count(&self) -> usize {
        self.inner
            .read()
            .expect("pool lock")
            .proxies
            .iter()
            .map(|proxy| proxy.tunnel.active_tunnels())
            .sum()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn proxy(key: &str) -> PoolProxy {
        PoolProxy {
            key: key.into(),
            tunnel: Tunnel::stub(key, 1),
            priority: 0,
        }
    }

    #[test]
    fn candidates_return_every_proxy() {
        let pool = ProxyPool::new(vec![proxy("a"), proxy("b"), proxy("c")]);
        let mut seen = std::collections::HashMap::new();
        for _ in 0..20 {
            let candidates = pool.candidates("example.com");
            assert_eq!(candidates.len(), 3);
            for candidate in candidates {
                seen.insert(candidate.key, ());
            }
        }
        for key in ["a", "b", "c"] {
            assert!(seen.contains_key(key), "proxy {key} never returned");
        }
    }

    #[test]
    fn candidates_rotate_between_calls() {
        let pool = ProxyPool::new(vec![proxy("a"), proxy("b")]);
        let first = pool.candidates("example.com")[0].key.clone();
        let second = pool.candidates("example.com")[0].key.clone();
        assert_ne!(first, second);
    }

    #[test]
    fn counts_report_targets_and_active_tunnels() {
        let pool = ProxyPool::new(vec![
            PoolProxy {
                tunnel: Tunnel::stub("a", 4),
                ..proxy("a")
            },
            proxy("b"),
        ]);
        assert_eq!(pool.count(), 2);
        assert_eq!(pool.tunnel_count(), 5);
        assert_eq!(pool.usable_count(), 0);
    }

    #[test]
    fn empty_pool_yields_nothing() {
        assert!(
            ProxyPool::new(Vec::new())
                .candidates("example.com")
                .is_empty()
        );
    }
}
