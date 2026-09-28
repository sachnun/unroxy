use std::time::Duration;

use base64::Engine;
use rsa::RsaPublicKey;
use rsa::pkcs1v15::{Signature, VerifyingKey};
use rsa::pkcs8::DecodePublicKey;
use rsa::signature::Verifier;
use sha2::{Digest, Sha256};

use crate::config::{
    REMOTE_SERVER_LIST_DECOMPRESSED_LIMIT, REMOTE_SERVER_LIST_LIMIT,
    REMOTE_SERVER_LIST_SIGNATURE_PUBLIC_KEY, REMOTE_SERVER_LIST_TIMEOUT, REMOTE_SERVER_LIST_URLS,
    SERVER_ENTRY_CACHE,
};

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("server list fetch: {0}")]
    Fetch(String),
    #[error("server list decode: {0}")]
    Decode(String),
    #[error("server list signature: {0}")]
    Signature(String),
    #[error("server list io: {0}")]
    Io(#[from] std::io::Error),
}

#[derive(serde::Deserialize)]
struct Package {
    data: String,
    #[serde(rename = "signingPublicKeyDigest")]
    digest: String,
    signature: String,
}

pub async fn load() -> String {
    match fetch().await {
        Ok(data) if !data.trim().is_empty() => {
            tracing::info!(
                "psiphon: fetched {} server entries from remote list",
                count(&data)
            );
            if let Err(err) = cache(&data) {
                tracing::warn!("psiphon: could not cache server entries: {err}");
            }
            data
        }
        Ok(_) => cached().unwrap_or_default(),
        Err(err) => {
            tracing::warn!("psiphon: remote server list fetch failed: {err}");
            cached().unwrap_or_default()
        }
    }
}

async fn fetch() -> Result<String, Error> {
    let mut last = Error::Fetch("no remote server list urls".to_string());
    for url in REMOTE_SERVER_LIST_URLS {
        match fetch_url(url).await {
            Ok(data) => return Ok(data),
            Err(err) => last = err,
        }
    }
    Err(last)
}

async fn fetch_url(url: &str) -> Result<String, Error> {
    let client = wreq::Client::builder()
        .emulation(crate::emulation::next())
        .timeout(REMOTE_SERVER_LIST_TIMEOUT)
        .build()
        .map_err(|err| Error::Fetch(err.to_string()))?;

    let response = client
        .get(url)
        .send()
        .await
        .map_err(|err| Error::Fetch(err.to_string()))?;
    if response.status() != 200 {
        return Err(Error::Fetch(format!("{url}: {}", response.status())));
    }
    let body = response
        .bytes()
        .await
        .map_err(|err| Error::Fetch(err.to_string()))?;
    if body.len() as u64 > REMOTE_SERVER_LIST_LIMIT {
        return Err(Error::Fetch(format!("{url}: response too large")));
    }

    decode_package(&body).map_err(|err| Error::Fetch(format!("{url}: {err}")))
}

fn decode_package(body: &[u8]) -> Result<String, Error> {
    use std::io::Read;

    let mut decoder =
        flate2::read::ZlibDecoder::new(body).take(REMOTE_SERVER_LIST_DECOMPRESSED_LIMIT);
    let mut json = String::new();
    decoder
        .read_to_string(&mut json)
        .map_err(|err| Error::Decode(err.to_string()))?;
    if json.len() as u64 >= REMOTE_SERVER_LIST_DECOMPRESSED_LIMIT {
        return Err(Error::Decode("server list too large".to_string()));
    }

    let package: Package =
        serde_json::from_str(&json).map_err(|err| Error::Decode(err.to_string()))?;

    let engine = base64::engine::general_purpose::STANDARD;
    let digest = Sha256::digest(REMOTE_SERVER_LIST_SIGNATURE_PUBLIC_KEY.as_bytes());
    let package_digest = engine
        .decode(&package.digest)
        .map_err(|err| Error::Signature(err.to_string()))?;
    if package_digest != digest.as_slice() {
        return Err(Error::Signature(
            "unexpected signing public key digest".to_string(),
        ));
    }

    let signature_bytes = engine
        .decode(&package.signature)
        .map_err(|err| Error::Signature(err.to_string()))?;
    let der = engine
        .decode(REMOTE_SERVER_LIST_SIGNATURE_PUBLIC_KEY)
        .map_err(|err| Error::Signature(err.to_string()))?;
    let key =
        RsaPublicKey::from_public_key_der(&der).map_err(|err| Error::Signature(err.to_string()))?;
    let verifying = VerifyingKey::<Sha256>::new(key);

    let signature = Signature::try_from(signature_bytes.as_slice())
        .map_err(|err| Error::Signature(err.to_string()))?;
    verifying
        .verify(package.data.as_bytes(), &signature)
        .map_err(|err| Error::Signature(err.to_string()))?;

    Ok(package.data)
}

fn cache(data: &str) -> Result<(), Error> {
    if let Some(parent) = std::path::Path::new(SERVER_ENTRY_CACHE).parent() {
        std::fs::create_dir_all(parent)?;
    }
    std::fs::write(SERVER_ENTRY_CACHE, data)?;
    Ok(())
}

fn cached() -> Option<String> {
    let data = std::fs::read_to_string(SERVER_ENTRY_CACHE).ok()?;
    if data.trim().is_empty() {
        None
    } else {
        tracing::info!("psiphon: using cached server entries");
        Some(data)
    }
}

pub fn count(data: &str) -> usize {
    data.lines().filter(|line| !line.trim().is_empty()).count()
}

#[allow(dead_code)]
fn unused(_: Duration) {}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn counts_entries() {
        assert_eq!(count("a\nb\n\n"), 2);
        assert_eq!(count(""), 0);
    }
}
