mod account;
mod provider;

use std::collections::{BTreeMap, HashMap};
use std::sync::Arc;
use std::time::Duration;

use crate::proxy::pool::Protocol;
use crate::proxy::{Forward, ForwardFactory, PoolProxy, Proxy, ProxyPool, Region};

use crate::forward::WreqFactory;
pub use provider::Config;

use provider::Accounts;

pub const SUPERVISE_INTERVAL: Duration = Duration::from_secs(5);

pub struct Vpn {
    accounts: Arc<Accounts>,
    factory: Arc<WreqFactory>,
}

impl Vpn {
    pub fn load(config: Config) -> Arc<Self> {
        Arc::new(Self {
            accounts: Arc::new(Accounts::load(config)),
            factory: Arc::new(WreqFactory),
        })
    }

    pub fn attach(&self, proxy: &Proxy) {
        let mut regions: BTreeMap<String, Arc<ProxyPool>> = BTreeMap::new();
        for account in self.accounts.iter() {
            let region = account.region().to_string();
            let pool = regions
                .entry(region.clone())
                .or_insert_with(|| ProxyPool::new(Vec::new()));
            if account.bridge_ready() {
                pool.push(proxy_for(account));
            }
        }

        for account in self.accounts.iter() {
            let account = Arc::clone(account);
            tokio::spawn(async move { account.warm().await });
        }

        let mut pools = HashMap::new();
        for (region, pool) in regions {
            let forward = self.factory.region_forward(&region, Arc::clone(&pool));
            let entry = Region::new(region.clone(), region.clone());
            entry.add(Protocol::Vpn, Arc::clone(&pool), forward);
            proxy.add_region(Arc::new(entry));
            pools.insert(region, pool);
        }

        self.supervise(pools);
    }

    fn supervise(&self, pools: HashMap<String, Arc<ProxyPool>>) {
        let accounts = Arc::clone(&self.accounts);
        tokio::spawn(async move {
            let mut ticker = tokio::time::interval(SUPERVISE_INTERVAL);
            ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
            loop {
                ticker.tick().await;
                for account in accounts.iter() {
                    let Some(pool) = pools.get(account.region()) else {
                        continue;
                    };
                    let key = format!("openvpn://{}", account.name());
                    let present = pool.contains(&key);
                    let usable = account.bridge_ready();
                    match (usable, present) {
                        (true, false) => {
                            pool.push(proxy_for(account));
                            tracing::info!(
                                "openvpn [{}]: joined {} pool",
                                account.name(),
                                account.region()
                            );
                        }
                        (false, true) => {
                            pool.remove(&key);
                            tracing::info!(
                                "openvpn [{}]: left {} pool",
                                account.name(),
                                account.region()
                            );
                        }
                        _ => {}
                    }
                }
            }
        });
    }

    pub fn protocol_route(
        &self,
        protocol: Protocol,
        pool: Arc<ProxyPool>,
    ) -> Option<Arc<dyn Forward>> {
        self.factory.protocol_forward(protocol, pool)
    }

    pub fn len(&self) -> usize {
        self.accounts.len()
    }

    pub fn spawn_reaper(&self) {
        self.accounts.spawn_reaper();
    }
}

fn proxy_for(account: &Arc<provider::Account>) -> PoolProxy {
    PoolProxy {
        key: Arc::from(format!("openvpn://{}", account.name())),
        tunnel: Arc::clone(account) as Arc<dyn crate::proxy::Tunnel>,
        priority: 0,
        protocol: Protocol::Vpn,
    }
}
