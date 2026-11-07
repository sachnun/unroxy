use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, RwLock};
use std::time::Duration;

use futures_util::future::BoxFuture;
use tokio::io::{AsyncRead, AsyncWrite};

use crate::proxy::exit::Exit;

pub trait Connection: AsyncRead + AsyncWrite + Send + Unpin {}

impl<T: AsyncRead + AsyncWrite + Send + Unpin> Connection for T {}

pub trait Tunnel: Send + Sync + 'static {
    fn target_pool(&self) -> usize;
    fn active_tunnels(&self) -> usize;
    fn is_ready(&self) -> bool;
    fn generation(&self) -> u64;
    fn proxy_url(&self) -> Option<String>;
    fn dial<'a>(
        &'a self,
        host: &'a str,
        port: u16,
    ) -> BoxFuture<'a, Result<Box<dyn Connection>, String>>;
    fn exit_for(&self, host: &str) -> Exit;
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum Protocol {
    Ssh,
    Vpn,
}

impl Protocol {
    pub fn parse(value: &str) -> Option<Self> {
        if value.eq_ignore_ascii_case("ssh") {
            Some(Self::Ssh)
        } else if value.eq_ignore_ascii_case("vpn") {
            Some(Self::Vpn)
        } else {
            None
        }
    }

    pub fn as_str(self) -> &'static str {
        match self {
            Self::Ssh => "ssh",
            Self::Vpn => "vpn",
        }
    }
}

#[derive(Clone)]
pub struct PoolProxy {
    pub key: Arc<str>,
    pub tunnel: Arc<dyn Tunnel>,
    pub priority: usize,
    pub protocol: Protocol,
}

#[derive(Clone)]
pub struct Candidate {
    pub key: Arc<str>,
    pub tunnel: Arc<dyn Tunnel>,
    pub priority: usize,
    pub protocol: Protocol,
}

impl Candidate {
    pub fn is_ready(&self) -> bool {
        self.tunnel.is_ready()
    }
}

struct Inner {
    proxies: Vec<PoolProxy>,
    children: Vec<Arc<ProxyPool>>,
}

pub struct ProxyPool {
    inner: RwLock<Inner>,
    rotation: AtomicU64,
}

impl ProxyPool {
    pub fn new(proxies: Vec<PoolProxy>) -> Arc<Self> {
        Arc::new(Self {
            inner: RwLock::new(Inner {
                proxies,
                children: Vec::new(),
            }),
            rotation: AtomicU64::new(0),
        })
    }

    pub fn candidates(&self, _target_host: &str) -> Vec<Candidate> {
        self.filtered(None, _target_host)
    }

    pub fn candidates_for(&self, protocol: Protocol, target_host: &str) -> Vec<Candidate> {
        self.filtered(Some(protocol), target_host)
    }

    fn filtered(&self, protocol: Option<Protocol>, _target_host: &str) -> Vec<Candidate> {
        let mut ready = self.local(protocol);
        let children = self.inner.read().expect("pool lock").children.clone();
        for child in children {
            for candidate in child.filtered(protocol, _target_host) {
                if !ready.iter().any(|known| known.key == candidate.key) {
                    ready.push(candidate);
                }
            }
        }

        ready.sort_by_key(|candidate| candidate.priority);

        if ready.len() > 1 {
            let offset =
                (self.rotation.fetch_add(1, Ordering::Relaxed) % ready.len() as u64) as usize;
            ready.rotate_left(offset);
        }
        ready
    }

    fn local(&self, protocol: Option<Protocol>) -> Vec<Candidate> {
        self.inner
            .read()
            .expect("pool lock")
            .proxies
            .iter()
            .filter(|proxy| protocol.is_none_or(|wanted| proxy.protocol == wanted))
            .map(|proxy| Candidate {
                key: proxy.key.clone(),
                tunnel: Arc::clone(&proxy.tunnel),
                priority: proxy.priority,
                protocol: proxy.protocol,
            })
            .collect()
    }

    pub fn add_child(&self, child: Arc<ProxyPool>) {
        let mut inner = self.inner.write().expect("pool lock");
        if !inner
            .children
            .iter()
            .any(|known| Arc::ptr_eq(known, &child))
        {
            inner.children.push(child);
        }
    }

    pub async fn pick(&self, target_host: &str) -> Option<Candidate> {
        self.select(None, target_host).await
    }

    async fn select(&self, protocol: Option<Protocol>, target_host: &str) -> Option<Candidate> {
        loop {
            let candidates = self.filtered(protocol, target_host);
            if candidates.is_empty() {
                return None;
            }
            if let Some(ready) = candidates.iter().find(|candidate| candidate.is_ready()) {
                return Some(ready.clone());
            }
            tokio::time::sleep(Duration::from_millis(200)).await;
        }
    }

    pub fn proxies(&self) -> Vec<PoolProxy> {
        self.inner.read().expect("pool lock").proxies.clone()
    }

    pub fn push(&self, proxy: PoolProxy) {
        let mut inner = self.inner.write().expect("pool lock");
        if !inner
            .proxies
            .iter()
            .any(|existing| existing.key == proxy.key)
        {
            inner.proxies.push(proxy);
        }
    }

    pub fn remove(&self, key: &str) {
        let mut inner = self.inner.write().expect("pool lock");
        inner.proxies.retain(|proxy| &*proxy.key != key);
    }

    pub fn contains(&self, key: &str) -> bool {
        self.all().iter().any(|proxy| &*proxy.key == key)
    }

    pub fn count(&self) -> usize {
        self.all().len()
    }

    fn all(&self) -> Vec<PoolProxy> {
        let mut proxies = self.proxies();
        let children = self.inner.read().expect("pool lock").children.clone();
        for child in children {
            for proxy in child.all() {
                if !proxies.iter().any(|known| known.key == proxy.key) {
                    proxies.push(proxy);
                }
            }
        }
        proxies
    }

    pub fn tunnel_count(&self) -> usize {
        self.all()
            .iter()
            .map(|proxy| proxy.tunnel.target_pool())
            .sum()
    }

    pub fn usable_count(&self) -> usize {
        self.all()
            .iter()
            .map(|proxy| proxy.tunnel.active_tunnels())
            .sum()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    struct Stub {
        target: usize,
        active: usize,
    }

    impl Tunnel for Stub {
        fn target_pool(&self) -> usize {
            self.target
        }

        fn active_tunnels(&self) -> usize {
            self.active
        }

        fn is_ready(&self) -> bool {
            self.active > 0
        }

        fn generation(&self) -> u64 {
            0
        }

        fn proxy_url(&self) -> Option<String> {
            None
        }

        fn dial<'a>(
            &'a self,
            _host: &'a str,
            _port: u16,
        ) -> BoxFuture<'a, Result<Box<dyn Connection>, String>> {
            Box::pin(async { Err("stub".to_string()) })
        }

        fn exit_for(&self, _host: &str) -> Exit {
            Exit::default()
        }
    }

    fn stub(target: usize) -> Arc<dyn Tunnel> {
        Arc::new(Stub { target, active: 0 })
    }

    fn proxy(key: &str) -> PoolProxy {
        PoolProxy {
            key: key.into(),
            tunnel: stub(1),
            priority: 0,
            protocol: Protocol::Ssh,
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
                tunnel: stub(4),
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

    #[test]
    fn filters_candidates_by_protocol() {
        let pool = ProxyPool::new(vec![
            proxy("ssh://a"),
            PoolProxy {
                protocol: Protocol::Vpn,
                ..proxy("openvpn://b")
            },
            proxy("ssh://c"),
        ]);
        let ssh = pool.candidates_for(Protocol::Ssh, "example.com");
        let vpn = pool.candidates_for(Protocol::Vpn, "example.com");
        assert_eq!(ssh.len(), 2);
        assert_eq!(vpn.len(), 1);
        assert!(ssh.iter().all(|c| c.protocol == Protocol::Ssh));
        assert_eq!(pool.candidates("example.com").len(), 3);
    }

    #[test]
    fn rotation_mixes_protocols() {
        let pool = ProxyPool::new(vec![
            proxy("ssh://a"),
            PoolProxy {
                protocol: Protocol::Vpn,
                ..proxy("openvpn://b")
            },
        ]);
        let mut seen = std::collections::HashSet::new();
        for _ in 0..8 {
            seen.insert(pool.candidates("example.com")[0].protocol);
        }
        assert_eq!(seen.len(), 2, "rotation should reach both protocols");
    }

    #[test]
    fn push_is_idempotent() {
        let pool = ProxyPool::new(Vec::new());
        pool.push(proxy("a"));
        pool.push(proxy("a"));
        assert_eq!(pool.count(), 1);
        assert!(pool.contains("a"));
    }

    #[test]
    fn remove_drops_a_proxy() {
        let pool = ProxyPool::new(vec![proxy("a"), proxy("b")]);
        pool.remove("a");
        assert_eq!(pool.count(), 1);
        assert!(!pool.contains("a"));
        assert!(pool.contains("b"));
    }

    #[test]
    fn children_are_visible_live() {
        let child = ProxyPool::new(vec![proxy("a")]);
        let parent = ProxyPool::new(Vec::new());
        parent.add_child(Arc::clone(&child));
        assert_eq!(parent.candidates("example.com").len(), 1);
        child.push(proxy("b"));
        assert_eq!(parent.candidates("example.com").len(), 2);
        assert_eq!(parent.count(), 2);
        assert!(parent.contains("b"));
    }

    #[test]
    fn push_grows_the_pool() {
        let pool = ProxyPool::new(vec![proxy("ssh://a")]);
        pool.push(PoolProxy {
            protocol: Protocol::Vpn,
            ..proxy("openvpn://b")
        });
        assert_eq!(pool.count(), 2);
    }
}
