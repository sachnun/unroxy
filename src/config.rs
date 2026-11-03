use std::sync::Arc;
use std::time::Duration;

use crate::proxy::{Provider, ProviderConfig, Proxy};
use crate::vpn::Config as VpnConfig;

use crate::forward::WreqFactory;
use crate::vpn::Vpn;

pub const DEFAULT_PORT: u16 = 8080;
pub const EXIT_CACHE_ENTRIES: usize = 4096;
pub const TUNNEL_REFRESH_INTERVAL: Duration = Duration::from_secs(600);
pub const STATE_REFRESH_INTERVAL: Duration = Duration::from_secs(2);
pub const TUNNEL_REFRESH_COUNT: usize = 1;
pub const DATA_DIR: &str = "/tmp/unroxy-psiphon";

pub fn provider_config() -> ProviderConfig {
    ProviderConfig {
        data_dir_root: DATA_DIR.to_string(),
        state_refresh_interval: STATE_REFRESH_INTERVAL,
        tunnel_refresh_interval: TUNNEL_REFRESH_INTERVAL,
        tunnel_refresh_count: TUNNEL_REFRESH_COUNT,
        client_platform: String::new(),
    }
}

pub fn vpn_config() -> VpnConfig {
    let connect_timeout = env_seconds("VPN_CONNECT_TIMEOUT", 20);
    let idle_timeout = env_seconds("VPN_IDLE_SECONDS", 300);
    VpnConfig {
        connect_timeout,
        idle_timeout,
        suffix: std::env::var("VPN_SUFFIX").unwrap_or_else(|_| "-VPN".to_string()),
    }
}

fn env_seconds(name: &str, default: u64) -> Duration {
    std::env::var(name)
        .ok()
        .and_then(|value| value.parse::<u64>().ok())
        .map(Duration::from_secs)
        .unwrap_or_else(|| Duration::from_secs(default))
}

pub async fn build() -> Arc<Proxy> {
    let factory = Arc::new(WreqFactory);
    let mut provider = Provider::new(factory, provider_config());
    let handler = provider.handler();

    if let Some(dir) = std::env::var("VPN_DIR").ok().filter(|dir| !dir.is_empty())
        && let Some(vpn) = Vpn::load(&dir, vpn_config())
    {
        vpn.attach(&handler);
        vpn.spawn_reaper();
        tracing::info!("vpn: {} accounts", vpn.len());
    }

    let raw = crate::serverlist::load().await;
    let regions = provider.start(&raw).await;
    tracing::info!("psiphon: {regions} regions online");
    handler
}
