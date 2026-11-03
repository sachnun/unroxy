use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use crate::psiphon::{Config, Tunnel as PsiphonTunnel};

use crate::proxy::pool::{PoolProxy, ProxyPool, Tunnel};
use crate::proxy::server::{Forward, Proxy, Region};

#[derive(Clone)]
pub struct ProviderConfig {
    pub data_dir_root: String,
    pub state_refresh_interval: Duration,
    pub tunnel_refresh_interval: Duration,
    pub tunnel_refresh_count: usize,
    pub client_platform: String,
}

impl Default for ProviderConfig {
    fn default() -> Self {
        Self {
            data_dir_root: "/tmp/unroxy-psiphon".to_string(),
            state_refresh_interval: Duration::from_secs(2),
            tunnel_refresh_interval: Duration::from_secs(600),
            tunnel_refresh_count: 1,
            client_platform: String::new(),
        }
    }
}

pub trait ForwardFactory: Send + Sync + 'static {
    fn region_forward(&self, region: &str, pool: Arc<ProxyPool>) -> Option<Arc<dyn Forward>>;
    fn default_forward(&self, pool: Arc<ProxyPool>) -> Option<Arc<dyn Forward>>;
}

pub struct Provider {
    handler: Arc<Proxy>,
    primaries: Arc<ProxyPool>,
    factory: Arc<dyn ForwardFactory>,
    config: ProviderConfig,
}

impl Tunnel for PsiphonTunnel {
    fn target_pool(&self) -> usize {
        PsiphonTunnel::target_pool(self)
    }

    fn active_tunnels(&self) -> usize {
        PsiphonTunnel::active_tunnels(self)
    }

    fn is_ready(&self) -> bool {
        PsiphonTunnel::is_ready(self)
    }

    fn generation(&self) -> u64 {
        PsiphonTunnel::generation(self)
    }

    fn proxy_url(&self) -> Option<String> {
        Some(PsiphonTunnel::proxy_url(self))
    }

    fn dial<'a>(
        &'a self,
        host: &'a str,
        port: u16,
    ) -> futures_util::future::BoxFuture<'a, Result<Box<dyn crate::proxy::pool::Connection>, String>>
    {
        Box::pin(async move {
            PsiphonTunnel::dial(self, host, port)
                .await
                .map(|stream| Box::new(stream) as Box<dyn crate::proxy::pool::Connection>)
                .map_err(|err| err.to_string())
        })
    }

    fn exit_for(&self, host: &str) -> crate::proxy::exit::Exit {
        let exit = PsiphonTunnel::exit_for(self, host);
        crate::proxy::exit::Exit {
            ip: exit.ip,
            region: exit.region,
            protocol: exit.protocol,
        }
    }
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

    pub async fn start(&mut self, raw_entries: &str) -> usize {
        if raw_entries.trim().is_empty() {
            tracing::warn!("psiphon: no server entries available, provider will be idle");
            return 0;
        }

        let by_region = crate::psiphon::group_by_region(raw_entries);
        let mut started = Vec::with_capacity(by_region.len());
        for (region, lines) in by_region {
            let entries_text = lines.join("\n");
            let target_pool = lines.len();
            let data_dir_root = self.config.data_dir_root.clone();
            let client_platform = self.config.client_platform.clone();
            match run_region(
                region.clone(),
                entries_text,
                target_pool,
                data_dir_root,
                client_platform,
            )
            .await
            {
                Ok(tunnel) => started.push((region, tunnel)),
                Err(err) => tracing::warn!("psiphon [{region}] init failed: {err}"),
            }
        }

        let mut targets = 0;
        for (region, tunnel) in &started {
            targets += tunnel.target_pool();
            self.add(region, Arc::clone(tunnel));
        }
        tracing::info!(
            "psiphon: {} regions, {} tunnels target",
            started.len(),
            targets
        );
        started.len()
    }

    pub fn add(&mut self, region: &str, tunnel: Arc<PsiphonTunnel>) {
        let key: Arc<str> = Arc::from(format!("psiphon://{region}"));
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
    entries_text: String,
    target_pool: usize,
    data_dir_root: String,
    client_platform: String,
) -> Result<Arc<PsiphonTunnel>, String> {
    tokio::task::spawn_blocking(move || {
        start_region(
            &region,
            &entries_text,
            target_pool,
            &data_dir_root,
            &client_platform,
        )
    })
    .await
    .map_err(|err| err.to_string())?
}

fn start_region(
    region: &str,
    entries_text: &str,
    target_pool: usize,
    data_dir_root: &str,
    client_platform: &str,
) -> Result<Arc<PsiphonTunnel>, String> {
    let data_dir = format!("{data_dir_root}-{region}");
    let mut config = Config {
        data_root_directory: data_dir.clone(),
        egress_region: region.to_string(),
        tunnel_pool_size: target_pool as u32,
        ..Config::default()
    };
    if !client_platform.is_empty() {
        config.client_platform = client_platform.to_string();
    }
    PsiphonTunnel::start(&config, entries_text, Path::new(&data_dir))
        .or_else(|_| PsiphonTunnel::start(&config, entries_text, Path::new(".")))
        .map(Arc::new)
        .map_err(|err| err.to_string())
}

fn spawn_refresh(config: ProviderConfig, region: String, tunnel: Arc<PsiphonTunnel>) {
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
                "psiphon [{region}]: rotated {} tunnel(s)",
                config.tunnel_refresh_count
            );
        }
    });
}
