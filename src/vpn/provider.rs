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
}

impl Default for Config {
    fn default() -> Self {
        Self {
            connect_timeout: Duration::from_secs(20),
            idle_timeout: Duration::from_secs(300),
        }
    }
}

pub struct Account {
    ip: String,
    region: String,
    profile: Profile,
    config: Config,
    tunnel: tokio::sync::Mutex<Option<Arc<Live>>>,
    socks_port: AtomicUsize,
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
    pub fn new(ip: String, file: AccountFile, config: Config) -> Result<Arc<Self>, Error> {
        let profile = file.profile(&ip)?;
        let region = if file.region.is_empty() {
            "UN".to_string()
        } else {
            file.region.clone()
        };
        Ok(Arc::new(Self {
            ip,
            region,
            profile,
            config,
            tunnel: tokio::sync::Mutex::new(None),
            socks_port: AtomicUsize::new(0),
            failures: AtomicUsize::new(0),
            not_before: AtomicU64::new(0),
            generation: AtomicU64::new(0),
            ready: AtomicBool::new(false),
        }))
    }

    pub fn name(&self) -> &str {
        &self.ip
    }

    pub fn region(&self) -> &str {
        &self.region
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
                let tunnel = Arc::new(tunnel);
                let socks_port = match spawn_socks(Arc::clone(&tunnel)).await {
                    Ok(port) => port,
                    Err(err) => {
                        tracing::warn!("openvpn [{}]: socks bridge {err}", self.ip);
                        return Err(err);
                    }
                };
                let live = Arc::new(Live {
                    tunnel,
                    connections: AtomicUsize::new(0),
                    idle_since: RwLock::new(None),
                });
                self.failures.store(0, Ordering::Relaxed);
                self.ready.store(true, Ordering::Relaxed);
                self.socks_port
                    .store(socks_port as usize, Ordering::Relaxed);
                tracing::info!("openvpn [{}]: up ({})", self.ip, live.tunnel.cipher());
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
                    self.ip,
                    backoff.as_secs()
                );
                Err(err.to_string())
            }
        }
    }

    pub fn bridge_ready(&self) -> bool {
        self.ready.load(Ordering::Relaxed) && self.socks_port.load(Ordering::Relaxed) != 0
    }

    pub async fn warm(&self) {
        if let Err(err) = self.live().await {
            tracing::debug!("openvpn [{}]: warm {err}", self.ip);
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
        self.socks_port.store(0, Ordering::Relaxed);
        *slot = None;
        tracing::debug!("openvpn [{}]: idle close", self.ip);
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
        usize::from(self.ready.load(Ordering::Relaxed))
    }

    fn is_ready(&self) -> bool {
        self.bridge_ready()
    }

    fn generation(&self) -> u64 {
        self.generation.load(Ordering::Relaxed)
    }

    fn proxy_url(&self) -> Option<String> {
        let port = self.socks_port.load(Ordering::Relaxed);
        if port == 0 {
            return None;
        }
        Some(format!("socks5h://127.0.0.1:{port}"))
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
            ip: self.ip.clone(),
            region: self.region.clone(),
            protocol: "openvpn".to_string(),
        }
    }
}

pub struct Accounts {
    order: Vec<Arc<Account>>,
}

impl Accounts {
    pub fn load(config: Config) -> Self {
        let mut order = Vec::new();
        for (ip, file) in crate::vpn::account::embedded() {
            match Account::new(ip.clone(), file, config.clone()) {
                Ok(account) => order.push(account),
                Err(err) => tracing::warn!("vpn {ip}: {err}"),
            }
        }
        Self { order }
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

async fn spawn_socks(tunnel: Arc<VpnTunnel>) -> Result<u16, String> {
    let listener = tokio::net::TcpListener::bind(("127.0.0.1", 0))
        .await
        .map_err(|err| err.to_string())?;
    let port = listener.local_addr().map_err(|err| err.to_string())?.port();
    tokio::spawn(async move {
        loop {
            let Ok((client, _)) = listener.accept().await else {
                return;
            };
            let tunnel = Arc::clone(&tunnel);
            tokio::spawn(async move {
                let _ = serve_socks(client, tunnel).await;
            });
        }
    });
    Ok(port)
}

async fn serve_socks(
    mut client: tokio::net::TcpStream,
    tunnel: Arc<VpnTunnel>,
) -> std::io::Result<()> {
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    let mut greeting = [0u8; 2];
    client.read_exact(&mut greeting).await?;
    if greeting[0] != 0x05 {
        return Ok(());
    }
    let mut methods = vec![0u8; greeting[1] as usize];
    client.read_exact(&mut methods).await?;
    client.write_all(&[0x05, 0x00]).await?;

    let mut request = [0u8; 4];
    client.read_exact(&mut request).await?;
    if request[0] != 0x05 || request[1] != 0x01 {
        return Ok(());
    }
    let host = match request[3] {
        0x01 => {
            let mut address = [0u8; 4];
            client.read_exact(&mut address).await?;
            std::net::Ipv4Addr::from(address).to_string()
        }
        0x04 => {
            let mut address = [0u8; 16];
            client.read_exact(&mut address).await?;
            std::net::Ipv6Addr::from(address).to_string()
        }
        0x03 => {
            let mut length = [0u8; 1];
            client.read_exact(&mut length).await?;
            let mut address = vec![0u8; length[0] as usize];
            client.read_exact(&mut address).await?;
            String::from_utf8_lossy(&address).into_owned()
        }
        _ => return Ok(()),
    };
    let mut port = [0u8; 2];
    client.read_exact(&mut port).await?;
    let port = u16::from_be_bytes(port);

    match tunnel.open(&host, port).await {
        Ok(mut remote) => {
            client
                .write_all(&[0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0])
                .await?;
            let _ = tokio::io::copy_bidirectional(&mut client, &mut remote).await;
        }
        Err(_) => {
            client
                .write_all(&[0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0])
                .await?;
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::vpn::account::Remote;

    fn account(ip: &str, region: &str) -> Arc<Account> {
        let file = AccountFile {
            region: region.to_string(),
            cipher: Some("AES-256-GCM".to_string()),
            auth: Some("SHA1".to_string()),
            ca: "-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----".to_string(),
            cert: "-----BEGIN CERTIFICATE-----\nBBB\n-----END CERTIFICATE-----".to_string(),
            key: "-----BEGIN PRIVATE KEY-----\nCCC\n-----END PRIVATE KEY-----".to_string(),
            remotes: vec![Remote {
                port: Some(1194),
                protocol: Some("udp".to_string()),
            }],
        };
        Account::new(ip.to_string(), file, Config::default()).expect("account")
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
    fn account_is_keyed_by_ip() {
        let account = account("103.6.52.13", "ID");
        assert_eq!(account.name(), "103.6.52.13");
        assert_eq!(account.region(), "ID");
        assert!(!Tunnel::is_ready(account.as_ref()));
        assert_eq!(account.target_pool(), 1);
        assert_eq!(account.active_tunnels(), 0);
        assert_eq!(account.generation(), 0);
        assert_eq!(account.proxy_url(), None);
    }

    #[test]
    fn failed_account_backs_off() {
        let account = account("103.6.52.13", "ID");
        account.failures.store(2, Ordering::Relaxed);
        account
            .not_before
            .store(now_millis() + 60_000, Ordering::Relaxed);
        assert!(!Tunnel::is_ready(account.as_ref()));
        account.not_before.store(0, Ordering::Relaxed);
        assert!(!Tunnel::is_ready(account.as_ref()));
        account.socks_port.store(1234, Ordering::Relaxed);
        account.ready.store(true, Ordering::Relaxed);
        assert!(Tunnel::is_ready(account.as_ref()));
    }

    #[test]
    fn exit_uses_region_and_protocol() {
        let account = account("103.6.52.13", "ID");
        let exit = Tunnel::exit_for(account.as_ref(), "example.com");
        assert_eq!(exit.ip, "103.6.52.13");
        assert_eq!(exit.region, "ID");
        assert_eq!(exit.protocol, "openvpn");
    }

    #[test]
    fn loads_embedded_accounts() {
        let accounts = Accounts::load(Config::default());
        assert!(accounts.len() > 100, "expected embedded accounts");
        let mut ips = std::collections::HashSet::new();
        for account in accounts.iter() {
            assert!(ips.insert(account.name().to_string()));
            assert!(!account.region.is_empty());
        }
    }
}
