use std::num::NonZeroUsize;
use std::sync::{Arc, RwLock};

use lru::LruCache;
use unroxy_psiphon::Exit;

use crate::config::EXIT_CACHE_ENTRIES;

#[derive(Clone)]
pub struct ExitCache {
    inner: Arc<RwLock<LruCache<String, Exit>>>,
}

impl Default for ExitCache {
    fn default() -> Self {
        let capacity = NonZeroUsize::new(EXIT_CACHE_ENTRIES).expect("non-zero cache capacity");
        Self {
            inner: Arc::new(RwLock::new(LruCache::new(capacity))),
        }
    }
}

impl ExitCache {
    pub fn record(&self, host: &str, exit: Exit) {
        if exit.is_empty() {
            return;
        }
        self.inner
            .write()
            .expect("exit lock")
            .put(host.to_lowercase(), exit);
    }

    pub fn get(&self, host: &str) -> Option<Exit> {
        self.inner
            .read()
            .expect("exit lock")
            .peek(&host.to_lowercase())
            .cloned()
    }
}
