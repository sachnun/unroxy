use std::time::Duration;

use base64::Engine;
use futures_util::future::BoxFuture;
use rsa::RsaPublicKey;
use rsa::pkcs1v15::{Signature, VerifyingKey};
use rsa::pkcs8::DecodePublicKey;
use rsa::signature::Verifier;
use sha2::{Digest, Sha256};

pub const REMOTE_SERVER_LIST_URLS: [&str; 1] =
    ["https://s3.amazonaws.com/psiphon/web/mjr4-p23r-puwl/server_list_compressed"];

pub const REMOTE_SERVER_LIST_SIGNATURE_PUBLIC_KEY: &str = "MIICIDANBgkqhkiG9w0BAQEFAAOCAg0AMIICCAKCAgEAt7Ls+/39r+T6zNW7GiVpJfzq/xvL9SBH5rIFnk0RXYEYavax3WS6HOD35eTAqn8AniOwiH+DOkvgSKF2caqk/y1dfq47Pdymtwzp9ikpB1C5OfAysXzBiwVJlCdajBKvBZDerV1cMvRzCKvKwRmvDmHgphQQ7WfXIGbRbmmk6opMBh3roE42KcotLFtqp0RRwLtcBRNtCdsrVsjiI1Lqz/lH+T61sGjSjQ3CHMuZYSQJZo/KrvzgQXpkaCTdbObxHqb6/+i1qaVOfEsvjoiyzTxJADvSytVtcTjijhPEV6XskJVHE1Zgl+7rATr/pDQkw6DPCNBS1+Y6fy7GstZALQXwEDN/qhQI9kWkHijT8ns+i1vGg00Mk/6J75arLhqcodWsdeG/M/moWgqQAnlZAGVtJI1OgeF5fsPpXu4kctOfuZlGjVZXQNW34aOzm8r8S0eVZitPlbhcPiR4gT/aSMz/wd8lZlzZYsje/Jr8u/YtlwjjreZrGRmG8KMOzukV3lLmMppXFMvl4bxv6YFEmIuTsOhbLTwFgh7KYNjodLj/LsqRVfwz31PgWQFTEPICV7GCvgVlPRxnofqKSjgTWI4mxDhBpVcATvaoBl1L/6WLbFvBsoAUBItWwctO2xalKxF5szhGm8lccoc5MZr8kfE0uxMgsxz4er68iCID+rsCAQM=";

pub const DEFAULT_CACHE_PATH: &str = "/tmp/unroxy-psiphon/server_entries.txt";
pub const RESPONSE_LIMIT: u64 = 16 << 20;
pub const DECOMPRESSED_LIMIT: u64 = 64 << 20;
pub const FETCH_TIMEOUT: Duration = Duration::from_secs(60);

pub trait HttpFetch: Send + Sync {
    fn get<'a>(&'a self, url: &'a str) -> BoxFuture<'a, Result<Vec<u8>, String>>;
}

pub async fn load(fetcher: &dyn HttpFetch, cache_path: &str) -> String {
    match fetch(fetcher).await {
        Ok(data) if !data.trim().is_empty() => {
            tracing::info!(
                "psiphon: fetched {} server entries from remote list",
                count(&data)
            );
            cache(cache_path, &data);
            data
        }
        Ok(_) => cached(cache_path).unwrap_or_default(),
        Err(err) => {
            tracing::warn!("psiphon: remote server list fetch failed: {err}");
            cached(cache_path).unwrap_or_default()
        }
    }
}

async fn fetch(fetcher: &dyn HttpFetch) -> Result<String, String> {
    let mut last = "no remote server list urls".to_string();
    for url in REMOTE_SERVER_LIST_URLS {
        match fetch_url(fetcher, url).await {
            Ok(data) => return Ok(data),
            Err(err) => last = err,
        }
    }
    Err(last)
}

async fn fetch_url(fetcher: &dyn HttpFetch, url: &str) -> Result<String, String> {
    let body = fetcher
        .get(url)
        .await
        .map_err(|err| format!("{url}: {err}"))?;
    if body.len() as u64 > RESPONSE_LIMIT {
        return Err(format!("{url}: response too large"));
    }
    decode_package(&body).map_err(|err| format!("{url}: {err}"))
}

pub fn decode_package(body: &[u8]) -> Result<String, String> {
    use std::io::Read;

    #[derive(serde::Deserialize)]
    struct Package {
        data: String,
        #[serde(rename = "signingPublicKeyDigest")]
        digest: String,
        signature: String,
    }

    let mut decoder = flate2::read::ZlibDecoder::new(body).take(DECOMPRESSED_LIMIT);
    let mut json = String::new();
    decoder
        .read_to_string(&mut json)
        .map_err(|err| format!("decompress: {err}"))?;
    if json.len() as u64 >= DECOMPRESSED_LIMIT {
        return Err("server list too large".to_string());
    }

    let package: Package = serde_json::from_str(&json).map_err(|err| format!("decode: {err}"))?;

    let engine = base64::engine::general_purpose::STANDARD;
    let digest = Sha256::digest(REMOTE_SERVER_LIST_SIGNATURE_PUBLIC_KEY.as_bytes());
    let package_digest = engine
        .decode(&package.digest)
        .map_err(|err| format!("signature digest: {err}"))?;
    if package_digest != digest.as_slice() {
        return Err("unexpected signing public key digest".to_string());
    }

    let signature_bytes = engine
        .decode(&package.signature)
        .map_err(|err| format!("signature: {err}"))?;
    let der = engine
        .decode(REMOTE_SERVER_LIST_SIGNATURE_PUBLIC_KEY)
        .map_err(|err| format!("public key: {err}"))?;
    let key =
        RsaPublicKey::from_public_key_der(&der).map_err(|err| format!("public key: {err}"))?;
    let verifying = VerifyingKey::<Sha256>::new(key);

    let signature = Signature::try_from(signature_bytes.as_slice())
        .map_err(|err| format!("signature: {err}"))?;
    verifying
        .verify(package.data.as_bytes(), &signature)
        .map_err(|err| format!("signature: {err}"))?;

    Ok(package.data)
}

pub fn count(data: &str) -> usize {
    data.lines().filter(|line| !line.trim().is_empty()).count()
}

fn cache(path: &str, data: &str) {
    if let Some(parent) = std::path::Path::new(path).parent()
        && std::fs::create_dir_all(parent).is_err()
    {
        return;
    }
    if let Err(err) = std::fs::write(path, data) {
        tracing::warn!("psiphon: could not cache server entries: {err}");
    }
}

fn cached(path: &str) -> Option<String> {
    let data = std::fs::read_to_string(path).ok()?;
    if data.trim().is_empty() {
        None
    } else {
        tracing::info!("psiphon: using cached server entries");
        Some(data)
    }
}
