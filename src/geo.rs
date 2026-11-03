use std::num::NonZeroUsize;
use std::sync::{Arc, LazyLock, Mutex};
use std::time::Duration;

use lru::LruCache;

use crate::config::EXIT_CACHE_ENTRIES;
use crate::emulation::next as next_emulation;

static ISP_CACHE: LazyLock<Mutex<LruCache<String, String>>> =
    LazyLock::new(|| Mutex::new(LruCache::new(capacity())));
static COUNTRY_CACHE: LazyLock<Mutex<LruCache<String, String>>> =
    LazyLock::new(|| Mutex::new(LruCache::new(capacity())));

fn capacity() -> NonZeroUsize {
    NonZeroUsize::new(EXIT_CACHE_ENTRIES).expect("non-zero cache capacity")
}

#[derive(Debug, Clone, Default)]
pub struct Lookup {
    pub isp: String,
    pub country: String,
}

const DEFAULT_BASE: &str = "https://ipwho.is";

static BASE: LazyLock<Mutex<Arc<str>>> = LazyLock::new(|| Mutex::new(Arc::from(DEFAULT_BASE)));

fn base() -> Arc<str> {
    Arc::clone(&BASE.lock().expect("base lock"))
}

#[cfg(test)]
fn set_base(base: &str) {
    *BASE.lock().expect("base lock") = Arc::from(base.trim_end_matches('/'));
}

pub const LOOKUP_TIMEOUT: Duration = Duration::from_secs(3);

pub async fn lookup_within(ip: &str, timeout: Duration) -> Lookup {
    if ip.is_empty() {
        return Lookup::default();
    }

    let cached_isp = ISP_CACHE.lock().expect("cache lock").get(ip).cloned();
    let cached_country = COUNTRY_CACHE.lock().expect("cache lock").get(ip).cloned();
    if let (Some(isp), Some(country)) = (cached_isp, cached_country) {
        return Lookup { isp, country };
    }

    let Ok(Ok(result)) = tokio::time::timeout(timeout, fetch_uncached(ip)).await else {
        return Lookup::default();
    };

    if !result.isp.is_empty() {
        let mut cache = ISP_CACHE.lock().expect("cache lock");
        cache.put(ip.to_string(), result.isp.clone());
    }
    if !result.country.is_empty() {
        let mut cache = COUNTRY_CACHE.lock().expect("cache lock");
        cache.put(ip.to_string(), result.country.clone());
    }
    result
}

async fn fetch_uncached(ip: &str) -> Result<Lookup, ()> {
    let client = wreq::Client::builder()
        .emulation(next_emulation())
        .timeout(Duration::from_secs(30))
        .build()
        .map_err(|_| ())?;
    fetch(&client, ip).await.ok_or(())
}

async fn fetch(client: &wreq::Client, ip: &str) -> Option<Lookup> {
    let response = client.get(format!("{}/{ip}", base())).send().await.ok()?;
    if response.status() != 200 {
        return None;
    }
    let text = response.text().await.ok()?;
    let body: serde_json::Value = serde_json::from_str(&text).ok()?;

    let mut isp = body["connection"]["isp"]
        .as_str()
        .unwrap_or_default()
        .to_string();
    if isp.is_empty() {
        isp = body["connection"]["org"]
            .as_str()
            .unwrap_or_default()
            .to_string();
    }

    let mut country = String::new();
    if body["success"].as_bool().unwrap_or(false) {
        let code = body["country_code"]
            .as_str()
            .unwrap_or_default()
            .trim()
            .to_ascii_uppercase();
        if code.len() == 2 && code.bytes().all(|byte| byte.is_ascii_uppercase()) {
            country = code;
        }
    }

    Some(Lookup { isp, country })
}

#[cfg(test)]
mod tests {
    use super::*;

    static SERIAL: tokio::sync::Mutex<()> = tokio::sync::Mutex::const_new(());

    async fn reset(base: &str) -> tokio::sync::MutexGuard<'static, ()> {
        let guard = SERIAL.lock().await;
        set_base(base);
        ISP_CACHE.lock().unwrap().clear();
        COUNTRY_CACHE.lock().unwrap().clear();
        guard
    }

    #[tokio::test]
    async fn empty_address_is_skipped() {
        assert_eq!(lookup_within("", LOOKUP_TIMEOUT).await.isp, "");
    }

    #[tokio::test]
    async fn reads_isp_and_country_and_caches_them() {
        let server = crate::testserver::serve(
            r#"{"success":true,"country_code":"sg","connection":{"isp":"ExampleNet","org":"ExampleOrg"}}"#,
        );
        let _guard = reset(&server.url).await;

        let first = lookup_within("203.0.113.9", LOOKUP_TIMEOUT).await;
        assert_eq!(first.isp, "ExampleNet");
        assert_eq!(first.country, "SG");

        let second = lookup_within("203.0.113.9", LOOKUP_TIMEOUT).await;
        assert_eq!(second.isp, "ExampleNet");
        assert_eq!(server.hits(), 1, "a cached lookup must not refetch");
    }

    #[tokio::test]
    async fn falls_back_to_org_when_isp_is_empty() {
        let server = crate::testserver::serve(
            r#"{"success":true,"country_code":"US","connection":{"isp":"","org":"FallbackOrg"}}"#,
        );
        let _guard = reset(&server.url).await;

        assert_eq!(
            lookup_within("198.51.100.4", LOOKUP_TIMEOUT).await.isp,
            "FallbackOrg"
        );
    }

    #[tokio::test]
    async fn rejects_unsuccessful_and_malformed_country_codes() {
        let failed = crate::testserver::serve(r#"{"success":false,"country_code":"US"}"#);
        let short = crate::testserver::serve(r#"{"success":true,"country_code":"S1"}"#);
        let garbage = crate::testserver::serve("not json");
        let _guard = reset(&failed.url).await;

        assert_eq!(
            lookup_within("203.0.113.1", LOOKUP_TIMEOUT).await.country,
            ""
        );

        set_base(&short.url);
        assert_eq!(
            lookup_within("203.0.113.2", LOOKUP_TIMEOUT).await.country,
            ""
        );

        set_base(&garbage.url);
        assert_eq!(lookup_within("203.0.113.3", LOOKUP_TIMEOUT).await.isp, "");
    }

    #[tokio::test]
    async fn failures_are_not_cached() {
        let server = crate::testserver::serve(r#"{"success":false,"country_code":"US"}"#);
        let _guard = reset(&server.url).await;
        lookup_within("203.0.113.4", LOOKUP_TIMEOUT).await;
        lookup_within("203.0.113.4", LOOKUP_TIMEOUT).await;
        assert_eq!(server.hits(), 2, "an empty result must be retried");
    }

    #[tokio::test]
    async fn a_slow_lookup_gives_up_at_the_timeout() {
        let server = crate::testserver::serve_slow(
            r#"{"success":true,"country_code":"US","connection":{"isp":"Late"}}"#,
            Duration::from_millis(500),
        );
        let _guard = reset(&server.url).await;

        let started = std::time::Instant::now();
        let result = lookup_within("203.0.113.5", Duration::from_millis(50)).await;
        assert_eq!(result.isp, "");
        assert!(started.elapsed() < Duration::from_millis(400));
    }
}
