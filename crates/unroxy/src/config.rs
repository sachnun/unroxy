use std::time::Duration;

use unroxy_proxy::ProviderConfig;

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
