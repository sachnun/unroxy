use std::sync::Arc;
use std::time::Duration;

use crate::proxy::pool::Protocol;
use crate::proxy::{Provider, ProviderConfig, Proxy, ProxyPool};
use crate::vpn::Config as VpnConfig;

use crate::forward::WreqFactory;
use crate::vpn::Vpn;

pub const DEFAULT_PORT: u16 = 8080;
pub const EXIT_CACHE_ENTRIES: usize = 4096;
pub const TUNNEL_REFRESH_INTERVAL: Duration = Duration::from_secs(600);
pub const STATE_REFRESH_INTERVAL: Duration = Duration::from_secs(2);
pub const TUNNEL_REFRESH_COUNT: usize = 1;
pub const VPN_CONNECT_TIMEOUT: Duration = Duration::from_secs(20);
pub const VPN_IDLE_TIMEOUT: Duration = Duration::from_secs(300);

pub fn provider_config() -> ProviderConfig {
    ProviderConfig {
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
    }
}

pub async fn build() -> Arc<Proxy> {
    let factory = Arc::new(WreqFactory);
    let mut provider = Provider::new(factory, provider_config());
    let handler = provider.handler();

    let vpn = Vpn::load(vpn_config());
    vpn.attach(&handler);
    vpn.spawn_reaper();
    tracing::info!("vpn: {} accounts", vpn.len());

    let regions = provider.start().await;
    tracing::info!("ssh: {regions} regions online");

    for protocol in [Protocol::Ssh, Protocol::Vpn] {
        let pool = ProxyPool::new(Vec::new());
        if let Some(forward) = vpn.protocol_route(protocol, Arc::clone(&pool)) {
            handler.set_protocol_route(protocol, pool, forward);
        }
    }

    tracing::info!("routes: {} regions", handler.stats().len());
    handler
}
