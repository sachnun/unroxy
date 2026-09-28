//! Starting one Psiphon tunnel per region, ported from
//! `internal/providers/psiphon/provider.go`.

use std::sync::Arc;
use std::time::Duration;

use unroxy_psiphon::{Config, Tunnel};

use crate::config::{STATE_REFRESH_INTERVAL, TUNNEL_REFRESH_COUNT, TUNNEL_REFRESH_INTERVAL};
use crate::entries;
use crate::pool::{Proxy, ProxyPool};
use crate::proxy::{Proxy as ProxyHandler, Region};

use crate::upstream::RotatingTransport;

pub struct Provider {
    pub handler: Arc<ProxyHandler>,
    /// Every region's tunnel is also a candidate for the unnamed default
    /// route, mirroring the Go host that sets each dialer as a primary.
    primaries: Arc<ProxyPool>,
}

impl Provider {
    pub fn new() -> Self {
        let primaries = ProxyPool::new(Vec::new());
        let default_transport = RotatingTransport::new(Arc::clone(&primaries));
        let handler = ProxyHandler::new(default_transport, Vec::new());
        Self { handler, primaries }
    }

    pub async fn load(&mut self) -> usize {
        let raw = crate::serverlist::load().await;
        if raw.trim().is_empty() {
            tracing::warn!("psiphon: no server entries available, provider will be idle");
            return 0;
        }

        let by_region = entries::parse_entries_by_region(&raw);
        let mut regions: Vec<(String, Vec<String>)> = by_region.into_iter().collect();
        regions.sort_by(|a, b| a.0.cmp(&b.0));

        // Every region starts at once, the way the Go provider spawns a
        // goroutine per region. Awaiting each start in turn would serialize
        // the datastore opens and delay the whole pool by minutes.
        let mut handles = Vec::with_capacity(regions.len());
        for (region, region_entries) in regions {
            let entries_text = join_entries(&region_entries);
            let target = region_entries.len();
            let region_for_task = region.clone();
            handles.push((
                region,
                tokio::task::spawn_blocking(move || {
                    start_region(&region_for_task, &entries_text, target)
                }),
            ));
        }

        let mut started = Vec::with_capacity(handles.len());
        for (region, handle) in handles {
            match handle.await {
                Ok(Ok(tunnel)) => started.push((region, tunnel)),
                Ok(Err(err)) => tracing::warn!("psiphon [{region}] init failed: {err}"),
                Err(err) => tracing::warn!("psiphon [{region}] task failed: {err}"),
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

    fn add(&mut self, region: &str, tunnel: Arc<Tunnel>) {
        let key: Arc<str> = Arc::from(format!("psiphon://{region}"));
        let pool = ProxyPool::new(vec![Proxy {
            key: Arc::clone(&key),
            tunnel: Arc::clone(&tunnel),
            priority: 0,
        }]);
        let transport = RotatingTransport::new(Arc::clone(&pool));

        let mut primaries = self.primaries.proxies();
        primaries.push(Proxy {
            key,
            tunnel: Arc::clone(&tunnel),
            priority: 0,
        });
        self.primaries.replace(primaries);

        self.handler.add_region(Region {
            name: region.to_string(),
            username: region.to_string(),
            pool,
            transport,
        });

        spawn_refresh(region.to_string(), tunnel);
    }
}

impl Default for Provider {
    fn default() -> Self {
        Self::new()
    }
}

fn join_entries(entries: &[String]) -> String {
    let mut out = String::new();
    for entry in entries {
        out.push_str(entry);
        out.push('\n');
    }
    out
}

fn start_region(
    region: &str,
    entries_text: &str,
    target_pool: usize,
) -> Result<Arc<Tunnel>, String> {
    let config = Config {
        data_root_directory: crate::config::data_dir(region),
        egress_region: region.to_string(),
        tunnel_pool_size: target_pool as u32,
        ..Config::default()
    };
    let data_dir = std::path::Path::new(&config.data_root_directory);
    Tunnel::start(&config, entries_text, data_dir)
        .map(Arc::new)
        .map_err(|err| err.to_string())
}

/// Retires one tunnel per interval so the controller replaces it, which is how
/// exits get rotated. The first retirement is jittered so regions do not all
/// refresh together.
fn spawn_refresh(region: String, tunnel: Arc<Tunnel>) {
    tokio::spawn(async move {
        // The counters are cached in the wrapper, so they are refreshed from
        // the moment the tunnel exists. A stale counter reads as "not ready",
        // which makes a dial wait out its readiness timeout.
        let mut ticker = tokio::time::interval(STATE_REFRESH_INTERVAL);
        ticker.tick().await;
        tunnel.refresh();

        // Rotation is staggered so regions do not all refresh their exits at
        // once, but the counters keep ticking regardless.
        let mut until_rotation =
            Duration::from_secs(rand::random_range(0..TUNNEL_REFRESH_INTERVAL.as_secs()));
        loop {
            ticker.tick().await;
            tunnel.refresh();
            if until_rotation > STATE_REFRESH_INTERVAL {
                until_rotation -= STATE_REFRESH_INTERVAL;
                continue;
            }
            until_rotation = TUNNEL_REFRESH_INTERVAL;
            if tunnel.active_tunnels() < tunnel.target_pool() {
                continue;
            }
            tunnel.terminate(TUNNEL_REFRESH_COUNT);
            tracing::debug!("psiphon [{region}]: rotated {TUNNEL_REFRESH_COUNT} tunnel(s)");
        }
    });
}
