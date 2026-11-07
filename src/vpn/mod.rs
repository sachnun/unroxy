mod account;
mod provider;

use std::collections::BTreeMap;
use std::sync::Arc;

use crate::proxy::pool::Protocol;
use crate::proxy::{Forward, ForwardFactory, PoolProxy, Proxy, ProxyPool, Region};

use crate::forward::WreqFactory;
pub use provider::Config;

use provider::Accounts;

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
        let mut regions: BTreeMap<String, Vec<PoolProxy>> = BTreeMap::new();
        for account in self.accounts.iter() {
            regions
                .entry(account.region().to_string())
                .or_default()
                .push(PoolProxy {
                    key: Arc::from(format!("openvpn://{}", account.name())),
                    tunnel: Arc::clone(account) as Arc<dyn crate::proxy::Tunnel>,
                    priority: 0,
                    protocol: Protocol::Vpn,
                });
        }

        for account in self.accounts.iter() {
            let account = Arc::clone(account);
            tokio::spawn(async move { account.warm().await });
        }

        for (region, proxies) in regions {
            let pool = ProxyPool::new(proxies);
            let forward = self.factory.region_forward(&region, Arc::clone(&pool));
            let entry = Region::new(region.clone(), region);
            entry.add(Protocol::Vpn, pool, forward);
            proxy.add_region(Arc::new(entry));
        }
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
