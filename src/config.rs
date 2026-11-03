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
pub const VPN_DIR: &str = "vpn";
pub const VPN_CONNECT_TIMEOUT: Duration = Duration::from_secs(20);
pub const VPN_IDLE_TIMEOUT: Duration = Duration::from_secs(300);
pub const VPN_SUFFIX: &str = "-VPN";

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
    VpnConfig {
        connect_timeout: VPN_CONNECT_TIMEOUT,
        idle_timeout: VPN_IDLE_TIMEOUT,
        suffix: VPN_SUFFIX.to_string(),
    }
}

pub async fn build() -> Arc<Proxy> {
    let factory = Arc::new(WreqFactory);
    let mut provider = Provider::new(factory, provider_config());
    let handler = provider.handler();

    if let Some(vpn) = Vpn::load(VPN_DIR, vpn_config()) {
        vpn.attach(&handler);
        vpn.spawn_reaper();
        tracing::info!("vpn: {} accounts", vpn.len());
    }

    let raw = crate::serverlist::load().await;
    let regions = provider.start(&raw).await;
    tracing::info!("psiphon: {regions} regions online");
    handler
}
