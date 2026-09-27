//! Exit identity per target host, replacing the Go server's global
//! `globalHostTunnels` map plus its reflective read of the dialed connection.

use std::collections::HashMap;
use std::sync::{Arc, RwLock};

use unroxy_psiphon::Exit;

#[derive(Clone, Default)]
pub struct ExitCache {
    inner: Arc<RwLock<HashMap<String, Exit>>>,
}

impl ExitCache {
    pub fn record(&self, host: &str, exit: Exit) {
        if exit.is_empty() {
            return;
        }
        self.inner
            .write()
            .expect("exit lock")
            .insert(host.to_lowercase(), exit);
    }

    pub fn get(&self, host: &str) -> Option<Exit> {
        self.inner
            .read()
            .expect("exit lock")
            .get(&host.to_lowercase())
            .cloned()
    }
}
