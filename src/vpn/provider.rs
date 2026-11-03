use std::path::Path;
use std::sync::atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, RwLock};
use std::time::{Duration, Instant};

use crate::openvpn::{Profile, Tunnel as VpnTunnel, TunnelConfig};
use crate::proxy::{Connection, Exit, Tunnel};
use futures_util::future::BoxFuture;
use tokio::io::{AsyncRead, AsyncWrite};

use crate::vpn::account::{Account as AccountFile, Error};

#[derive(Debug, Clone)]
pub struct Config {
    pub connect_timeout: Duration,
    pub idle_timeout: Duration,
    pub suffix: String,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            connect_timeout: Duration::from_secs(20),
            idle_timeout: Duration::from_secs(300),
            suffix: "-VPN".to_string(),
        }
    }
}

pub struct Account {
    name: String,
    profile: Profile,
    config: Config,
    tunnel: tokio::sync::Mutex<Option<Arc<Live>>>,
    failures: AtomicUsize,
    not_before: AtomicU64,
    generation: AtomicU64,
    ready: AtomicBool,
}

struct Live {
    tunnel: Arc<VpnTunnel>,
    connections: AtomicUsize,
    idle_since: RwLock<Option<Instant>>,
}

impl Account {
    pub fn new(file: AccountFile, config: Config) -> Result<Arc<Self>, Error> {
        let profile = file.profile()?;
        let name = format!("{}{}", file.name, config.suffix);
        Ok(Arc::new(Self {
            name,
            profile,
            config,
            tunnel: tokio::sync::Mutex::new(None),
            failures: AtomicUsize::new(0),
            not_before: AtomicU64::new(0),
            generation: AtomicU64::new(0),
            ready: AtomicBool::new(false),
        }))
    }

    pub fn name(&self) -> &str {
        &self.name
    }

    fn backoff(&self) -> Duration {
        backoff_delay(self.failures.load(Ordering::Relaxed))
    }

    async fn live(&self) -> Result<Arc<Live>, String> {
        let mut slot = self.tunnel.lock().await;
        if let Some(live) = slot.as_ref() {
            *live.idle_since.write().expect("idle lock") = None;
            return Ok(Arc::clone(live));
        }
        let now = now_millis();
        if now < self.not_before.load(Ordering::Relaxed) {
            return Err("backoff".to_string());
        }
        self.connect(&mut slot, now).await
    }

    async fn connect(&self, slot: &mut Option<Arc<Live>>, now: u64) -> Result<Arc<Live>, String> {
        let config = TunnelConfig {
            connect_timeout: self.config.connect_timeout,
        };
        match VpnTunnel::connect(&self.profile, config).await {
            Ok(tunnel) => {
                let live = Arc::new(Live {
                    tunnel: Arc::new(tunnel),
                    connections: AtomicUsize::new(0),
                    idle_since: RwLock::new(None),
                });
                self.failures.store(0, Ordering::Relaxed);
                self.ready.store(true, Ordering::Relaxed);
                tracing::info!("openvpn [{}]: up ({})", self.name, live.tunnel.cipher());
                *slot = Some(Arc::clone(&live));
                Ok(live)
            }
            Err(err) => {
                self.failures.fetch_add(1, Ordering::Relaxed);
                let backoff = self.backoff();
                self.not_before
                    .store(now + backoff.as_millis() as u64, Ordering::Relaxed);
                self.ready.store(false, Ordering::Relaxed);
                self.generation.fetch_add(1, Ordering::Relaxed);
                tracing::warn!(
                    "openvpn [{}]: {err} (retry in {}s)",
                    self.name,
                    backoff.as_secs()
                );
                Err(err.to_string())
            }
        }
    }

    pub async fn open(&self, host: &str, port: u16) -> Result<Box<dyn Connection>, String> {
        let live = self.live().await?;
        let stream = live
            .tunnel
            .open(host, port)
            .await
            .map_err(|err| err.to_string())?;
        live.connections.fetch_add(1, Ordering::Relaxed);
        Ok(Box::new(Guard {
            stream: Some(stream),
            live,
        }))
    }

    pub async fn reap(&self) {
        let mut slot = self.tunnel.lock().await;
        let Some(live) = slot.as_ref() else {
            return;
        };
        if !idle_expired(
            &live.connections,
            &live.idle_since,
            Instant::now(),
            self.config.idle_timeout,
        ) {
            if live.connections.load(Ordering::Relaxed) > 0 {
                *live.idle_since.write().expect("idle lock") = None;
            }
            return;
        }
        *live.idle_since.write().expect("idle lock") = None;
        self.ready.store(false, Ordering::Relaxed);
        self.generation.fetch_add(1, Ordering::Relaxed);
        *slot = None;
        tracing::debug!("openvpn [{}]: idle close", self.name);
    }

    pub fn is_ready(&self) -> bool {
        self.ready.load(Ordering::Relaxed)
    }
}

struct Guard {
    stream: Option<crate::openvpn::Stream>,
    live: Arc<Live>,
}

impl Drop for Guard {
    fn drop(&mut self) {
        self.stream.take();
        self.live.connections.fetch_sub(1, Ordering::Relaxed);
        if self.live.connections.load(Ordering::Relaxed) == 0 {
            *self.live.idle_since.write().expect("idle lock") = Some(Instant::now());
        }
    }
}

impl AsyncRead for Guard {
    fn poll_read(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
        buf: &mut tokio::io::ReadBuf<'_>,
    ) -> std::task::Poll<std::io::Result<()>> {
        match self.stream.as_mut() {
            Some(stream) => std::pin::Pin::new(stream).poll_read(cx, buf),
            None => std::task::Poll::Ready(Ok(())),
        }
    }
}

impl AsyncWrite for Guard {
    fn poll_write(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
        buf: &[u8],
    ) -> std::task::Poll<std::io::Result<usize>> {
        match self.stream.as_mut() {
            Some(stream) => std::pin::Pin::new(stream).poll_write(cx, buf),
            None => std::task::Poll::Ready(Err(std::io::Error::other("stream closed"))),
        }
    }

    fn poll_flush(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<std::io::Result<()>> {
        match self.stream.as_mut() {
            Some(stream) => std::pin::Pin::new(stream).poll_flush(cx),
            None => std::task::Poll::Ready(Ok(())),
        }
    }

    fn poll_shutdown(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<std::io::Result<()>> {
        match self.stream.as_mut() {
            Some(stream) => std::pin::Pin::new(stream).poll_shutdown(cx),
            None => std::task::Poll::Ready(Ok(())),
        }
    }
}

impl Tunnel for Account {
    fn target_pool(&self) -> usize {
        1
    }

    fn active_tunnels(&self) -> usize {
        usize::from(self.is_ready())
    }

    fn is_ready(&self) -> bool {
        self.is_ready()
    }

    fn generation(&self) -> u64 {
        self.generation.load(Ordering::Relaxed)
    }

    fn proxy_url(&self) -> Option<String> {
        None
    }

    fn dial<'a>(
        &'a self,
        host: &'a str,
        port: u16,
    ) -> BoxFuture<'a, Result<Box<dyn Connection>, String>> {
        Box::pin(async move { self.open(host, port).await })
    }

    fn exit_for(&self, _host: &str) -> Exit {
        Exit {
            ip: String::new(),
            region: self.name().to_string(),
            protocol: "openvpn".to_string(),
        }
    }
}

pub struct Accounts {
    order: Vec<Arc<Account>>,
}

impl Accounts {
    pub fn load(dir: &Path, config: Config) -> Result<Self, Error> {
        let mut order = Vec::new();
        for entry in std::fs::read_dir(dir)?.flatten() {
            let path = entry.path();
            let file = match path.extension().and_then(|ext| ext.to_str()) {
                Some("ovpn") => AccountFile::from_ovpn(&path),
                Some("toml") => AccountFile::from_toml(&path),
                _ => continue,
            };
            let file = match file {
                Ok(file) => file,
                Err(err) => {
                    tracing::warn!("vpn {}: {err}", path.display());
                    continue;
                }
            };
            let account = match Account::new(file, config.clone()) {
                Ok(account) => account,
                Err(err) => {
                    tracing::warn!("vpn {}: {err}", path.display());
                    continue;
                }
            };
            let name = account.name().to_string();
            if order
                .iter()
                .any(|existing: &Arc<Account>| existing.name() == name)
            {
                continue;
            }
            order.push(account);
        }
        Ok(Self { order })
    }

    pub fn len(&self) -> usize {
        self.order.len()
    }

    pub fn iter(&self) -> impl Iterator<Item = &Arc<Account>> {
        self.order.iter()
    }

    pub fn spawn_reaper(self: &Arc<Self>) {
        let accounts = Arc::clone(self);
        tokio::spawn(async move {
            let mut ticker = tokio::time::interval(Duration::from_secs(30));
            loop {
                ticker.tick().await;
                for account in accounts.iter() {
                    account.reap().await;
                }
            }
        });
    }
}

fn idle_expired(
    connections: &AtomicUsize,
    idle_since: &RwLock<Option<Instant>>,
    now: Instant,
    timeout: Duration,
) -> bool {
    if connections.load(Ordering::Relaxed) > 0 {
        return false;
    }
    let mut idle = idle_since.write().expect("idle lock");
    let since = idle.get_or_insert(now);
    now.duration_since(*since) >= timeout
}

fn backoff_delay(failures: usize) -> Duration {
    Duration::from_secs(1u64 << failures.min(8))
}

fn now_millis() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|elapsed| elapsed.as_millis() as u64)
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::vpn::account::Remote;

    fn account(name: &str) -> Arc<Account> {
        let file = AccountFile {
            name: name.to_string(),
            protocol: Some("udp".to_string()),
            cipher: Some("AES-256-GCM".to_string()),
            auth: Some("SHA1".to_string()),
            ca: "-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----".to_string(),
            cert: "-----BEGIN CERTIFICATE-----\nBBB\n-----END CERTIFICATE-----".to_string(),
            key: "-----BEGIN PRIVATE KEY-----\nCCC\n-----END PRIVATE KEY-----".to_string(),
            remotes: vec![Remote {
                host: "198.51.100.1".to_string(),
                port: Some(1194),
                protocol: Some("udp".to_string()),
            }],
        };
        Account::new(file, Config::default()).expect("account")
    }

    fn idle(connections: usize, since: Option<Instant>) -> (AtomicUsize, RwLock<Option<Instant>>) {
        (AtomicUsize::new(connections), RwLock::new(since))
    }

    #[test]
    fn idle_close_waits_for_timeout() {
        let now = Instant::now();
        let timeout = Duration::from_secs(300);
        let (connections, since) = idle(0, Some(now - Duration::from_secs(299)));
        assert!(!idle_expired(&connections, &since, now, timeout));
        let (connections, since) = idle(0, Some(now - Duration::from_secs(300)));
        assert!(idle_expired(&connections, &since, now, timeout));
    }

    #[test]
    fn idle_close_skips_active_connections() {
        let now = Instant::now();
        let (connections, since) = idle(1, Some(now - Duration::from_secs(3600)));
        assert!(!idle_expired(
            &connections,
            &since,
            now,
            Duration::from_secs(300)
        ));
    }

    #[test]
    fn idle_clock_starts_on_first_reap() {
        let now = Instant::now();
        let (connections, since) = idle(0, None);
        assert!(!idle_expired(
            &connections,
            &since,
            now,
            Duration::from_secs(300)
        ));
        assert!(since.read().expect("idle lock").is_some());
        assert!(idle_expired(
            &connections,
            &since,
            now + Duration::from_secs(301),
            Duration::from_secs(300)
        ));
    }

    #[test]
    fn backoff_grows_and_caps() {
        assert_eq!(backoff_delay(0), Duration::from_secs(1));
        assert_eq!(backoff_delay(3), Duration::from_secs(8));
        assert_eq!(backoff_delay(8), Duration::from_secs(256));
        assert_eq!(backoff_delay(64), Duration::from_secs(256));
    }

    #[test]
    fn name_gets_region_suffix() {
        let account = account("SG");
        assert_eq!(account.name(), "SG-VPN");
        assert!(!account.is_ready());
        assert_eq!(account.target_pool(), 1);
        assert_eq!(account.active_tunnels(), 0);
        assert_eq!(account.generation(), 0);
        assert_eq!(account.proxy_url(), None);
    }

    #[test]
    fn exit_uses_region_and_protocol() {
        let account = account("ID");
        let exit = Tunnel::exit_for(account.as_ref(), "example.com");
        assert_eq!(exit.region, "ID-VPN");
        assert_eq!(exit.protocol, "openvpn");
        assert!(exit.ip.is_empty());
    }

    #[test]
    fn loads_profiles_from_directory() {
        let dir = std::env::temp_dir().join(format!("unroxy-vpn-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).expect("dir");
        std::fs::write(
            dir.join("[SG] Server 1.ovpn"),
            "proto udp\nremote 198.51.100.1 1194\ncipher AES-128-CBC\nauth SHA1\n\
             <ca>\n-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----\n</ca>\n\
             <cert>\n-----BEGIN CERTIFICATE-----\nBBB\n-----END CERTIFICATE-----\n</cert>\n\
             <key>\n-----BEGIN PRIVATE KEY-----\nCCC\n-----END PRIVATE KEY-----\n</key>\n",
        )
        .expect("write");
        std::fs::write(dir.join("[SG] Server 2.ovpn"), "remote 198.51.100.2\n")
            .expect("write duplicate");
        std::fs::write(dir.join("notes.txt"), "ignored").expect("write");
        let accounts = Accounts::load(&dir, Config::default()).expect("accounts");
        assert_eq!(accounts.len(), 1, "duplicate region is skipped");
        assert_eq!(accounts.iter().next().expect("account").name(), "SG-VPN");
        let _ = std::fs::remove_dir_all(&dir);
    }
}
