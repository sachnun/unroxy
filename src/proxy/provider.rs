use std::sync::Arc;
use std::time::Duration;

use crate::proxy::pool::{Connection, PoolProxy, ProxyPool, Tunnel};
use crate::proxy::server::{Forward, Proxy, Region};
use crate::ssh::{Config, Tunnel as SshTunnel};

pub trait ForwardFactory: Send + Sync + 'static {
    fn region_forward(&self, region: &str, pool: Arc<ProxyPool>) -> Option<Arc<dyn Forward>>;
    fn default_forward(&self, pool: Arc<ProxyPool>) -> Option<Arc<dyn Forward>>;
}

#[derive(Clone)]
pub struct ProviderConfig {
    pub state_refresh_interval: Duration,
    pub tunnel_refresh_interval: Duration,
    pub tunnel_refresh_count: usize,
    pub client_platform: String,
}

impl Default for ProviderConfig {
    fn default() -> Self {
        Self {
            state_refresh_interval: Duration::from_secs(2),
            tunnel_refresh_interval: Duration::from_secs(600),
            tunnel_refresh_count: 1,
            client_platform: String::new(),
        }
    }
}

impl Tunnel for SshTunnel {
    fn target_pool(&self) -> usize {
        SshTunnel::target_pool(self)
    }

    fn active_tunnels(&self) -> usize {
        SshTunnel::active_tunnels(self)
    }

    fn is_ready(&self) -> bool {
        SshTunnel::is_ready(self)
    }

    fn generation(&self) -> u64 {
        SshTunnel::generation(self)
    }

    fn proxy_url(&self) -> Option<String> {
        Some(SshTunnel::proxy_url(self))
    }

    fn dial<'a>(
        &'a self,
        host: &'a str,
        port: u16,
    ) -> futures_util::future::BoxFuture<'a, Result<Box<dyn Connection>, String>> {
        Box::pin(async move {
            SshTunnel::dial(self, host, port)
                .await
                .map(|stream| Box::new(stream) as Box<dyn Connection>)
                .map_err(|err| err.to_string())
        })
    }

    fn exit_for(&self, host: &str) -> crate::proxy::exit::Exit {
        let exit = SshTunnel::exit_for(self, host);
        crate::proxy::exit::Exit {
            ip: exit.ip,
            region: exit.region,
            protocol: exit.protocol,
        }
    }
}

pub struct Provider {
    handler: Arc<Proxy>,
    primaries: Arc<ProxyPool>,
    factory: Arc<dyn ForwardFactory>,
    config: ProviderConfig,
}

impl Provider {
    pub fn new(factory: Arc<dyn ForwardFactory>, config: ProviderConfig) -> Self {
        let primaries = ProxyPool::new(Vec::new());
        let default_forward = factory.default_forward(Arc::clone(&primaries));
        let handler = Proxy::new(Arc::clone(&primaries), default_forward);
        Self {
            handler,
            primaries,
            factory,
            config,
        }
    }

    pub fn handler(&self) -> Arc<Proxy> {
        Arc::clone(&self.handler)
    }

    pub async fn start(&mut self) -> usize {
        let by_region = crate::ssh::group_by_region(crate::ssh::accounts());
        if by_region.is_empty() {
            tracing::warn!("ssh: no accounts available, provider will be idle");
            return 0;
        }

        let mut started = Vec::with_capacity(by_region.len());
        for (region, accounts) in by_region {
            let target_pool = accounts.len();
            let client_platform = self.config.client_platform.clone();
            match run_region(region.clone(), target_pool, client_platform).await {
                Ok(tunnel) => started.push((region, tunnel)),
                Err(err) => tracing::warn!("ssh [{region}] init failed: {err}"),
            }
        }

        let mut targets = 0;
        for (region, tunnel) in &started {
            targets += tunnel.target_pool();
            self.add(region, Arc::clone(tunnel));
        }
        tracing::info!("ssh: {} regions, {} tunnels target", started.len(), targets);
        started.len()
    }

    pub fn add(&mut self, region: &str, tunnel: Arc<SshTunnel>) {
        let key: Arc<str> = Arc::from(format!("ssh://{region}"));
        let pool = ProxyPool::new(vec![PoolProxy {
            key: Arc::clone(&key),
            tunnel: Arc::clone(&tunnel) as Arc<dyn Tunnel>,
            priority: 0,
        }]);
        let forward = self.factory.region_forward(region, Arc::clone(&pool));
        self.handler.add_region(Arc::new(Region {
            name: region.to_string(),
            username: region.to_string(),
            pool,
            forward,
        }));

        let mut primaries = self.primaries.proxies();
        primaries.push(PoolProxy {
            key,
            tunnel: Arc::clone(&tunnel) as Arc<dyn Tunnel>,
            priority: 0,
        });
        self.primaries.replace(primaries);

        spawn_refresh(self.config.clone(), region.to_string(), tunnel);
    }
}

async fn run_region(
    region: String,
    target_pool: usize,
    client_platform: String,
) -> Result<Arc<SshTunnel>, String> {
    tokio::task::spawn_blocking(move || start_region(&region, target_pool, &client_platform))
        .await
        .map_err(|err| err.to_string())?
}

fn start_region(
    region: &str,
    target_pool: usize,
    client_platform: &str,
) -> Result<Arc<SshTunnel>, String> {
    let mut config = Config {
        region: region.to_string(),
        pool_size: target_pool as u32,
        ..Config::default()
    };
    if !client_platform.is_empty() {
        config.client_platform = client_platform.to_string();
    }
    SshTunnel::start(&config)
        .map(Arc::new)
        .map_err(|err| err.to_string())
}

fn spawn_refresh(config: ProviderConfig, region: String, tunnel: Arc<SshTunnel>) {
    tokio::spawn(async move {
        let mut ticker = tokio::time::interval(config.state_refresh_interval);
        ticker.tick().await;

        let seconds = config.tunnel_refresh_interval.as_secs().max(1);
        let mut until_rotation = Duration::from_secs(rand::random_range(0..seconds));
        loop {
            ticker.tick().await;
            if until_rotation > config.state_refresh_interval {
                until_rotation -= config.state_refresh_interval;
                continue;
            }
            until_rotation = config.tunnel_refresh_interval;
            if tunnel.active_tunnels() < tunnel.target_pool() {
                continue;
            }
            tunnel.terminate(config.tunnel_refresh_count);
            tracing::debug!(
                "ssh [{region}]: rotated {} tunnel(s)",
                config.tunnel_refresh_count
            );
        }
    });
}
