mod account;
mod provider;

use std::sync::Arc;

use crate::proxy::{ForwardFactory, PoolProxy, Proxy, ProxyPool, Region};

use crate::dial::DialFactory;
pub use provider::Config;

use provider::Accounts;

pub struct Vpn {
    accounts: Arc<Accounts>,
    factory: Arc<DialFactory>,
}

impl Vpn {
    pub fn load(dir: &str, config: Config) -> Option<Arc<Self>> {
        if dir.trim().is_empty() {
            return None;
        }
        let path = std::path::Path::new(dir);
        if !path.is_dir() {
            tracing::warn!("vpn: {dir} is not a directory");
            return None;
        }
        match Accounts::load(path, config) {
            Ok(accounts) => Some(Arc::new(Self {
                accounts: Arc::new(accounts),
                factory: Arc::new(DialFactory),
            })),
            Err(err) => {
                tracing::warn!("vpn: {err}");
                None
            }
        }
    }

    pub fn attach(&self, proxy: &Proxy) {
        for account in self.accounts.iter() {
            let pool = ProxyPool::new(vec![PoolProxy {
                key: Arc::from(format!("openvpn://{}", account.name())),
                tunnel: Arc::clone(account) as Arc<dyn crate::proxy::Tunnel>,
                priority: 0,
            }]);
            let forward = self
                .factory
                .region_forward(account.name(), Arc::clone(&pool));
            proxy.add_region(Arc::new(Region {
                name: account.name().to_string(),
                username: account.name().to_string(),
                pool,
                forward,
            }));
        }
    }

    pub fn len(&self) -> usize {
        self.accounts.len()
    }

    pub fn spawn_reaper(&self) {
        self.accounts.spawn_reaper();
    }
}
