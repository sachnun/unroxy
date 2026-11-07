use std::collections::BTreeMap;

use include_dir::{Dir, include_dir};
use serde::Deserialize;

use crate::openvpn::{Auth, Cipher, Credentials, Profile, Protocol, Server};

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("account: {0}")]
    Account(String),
}

#[derive(Debug, Clone, Deserialize)]
pub struct Account {
    #[serde(default)]
    pub region: String,
    #[serde(default)]
    pub cipher: Option<String>,
    #[serde(default)]
    pub auth: Option<String>,
    #[serde(default)]
    pub ca: String,
    #[serde(default)]
    pub cert: String,
    #[serde(default)]
    pub key: String,
    #[serde(default)]
    pub remotes: Vec<Remote>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct Remote {
    #[serde(default)]
    pub port: Option<u16>,
    #[serde(default)]
    pub protocol: Option<String>,
}

static ACCOUNTS: Dir<'static> = include_dir!("$CARGO_MANIFEST_DIR/accounts/vpn");

pub fn embedded() -> BTreeMap<String, Account> {
    let mut accounts = BTreeMap::new();
    for file in ACCOUNTS.files() {
        let Some(ip) = file
            .path()
            .file_stem()
            .and_then(|stem| stem.to_str())
            .map(str::to_string)
        else {
            continue;
        };
        let Some(text) = file.contents_utf8() else {
            continue;
        };
        match parse(text) {
            Ok(account) => {
                accounts.insert(ip, account);
            }
            Err(err) => tracing::warn!("vpn account {ip}: {err}"),
        }
    }
    accounts
}

pub fn parse(text: &str) -> Result<Account, Error> {
    toml::from_str(text).map_err(|err| Error::Account(err.to_string()))
}

impl Account {
    pub fn profile(&self, host: &str) -> Result<Profile, Error> {
        let cipher = match self.cipher.as_deref() {
            Some(name) => {
                Cipher::parse(name).ok_or_else(|| Error::Account(format!("cipher {name}")))?
            }
            None => Cipher::parse("AES-256-GCM").expect("default cipher"),
        };
        let auth = self
            .auth
            .as_deref()
            .and_then(Auth::parse)
            .unwrap_or(Auth::Sha1);
        let mut servers = Vec::with_capacity(self.remotes.len().max(1));
        for remote in &self.remotes {
            servers.push(Server {
                host: host.to_string(),
                port: remote.port.unwrap_or(1194),
                protocol: protocol_of(remote.protocol.as_deref()),
            });
        }
        if servers.is_empty() {
            servers.push(Server {
                host: host.to_string(),
                port: 1194,
                protocol: Protocol::Udp,
            });
        }
        servers.sort_by_key(|server| server.protocol != Protocol::Udp);
        servers.dedup_by(|a, b| a.port == b.port && a.protocol == b.protocol);
        Ok(Profile {
            servers,
            cipher,
            auth,
            credentials: Credentials {
                ca: pem(&self.ca, "CERTIFICATE"),
                cert: pem(&self.cert, "CERTIFICATE"),
                key: pem(&self.key, "PRIVATE KEY"),
            },
        })
    }
}

fn protocol_of(value: Option<&str>) -> Protocol {
    match value {
        Some("tcp") | Some("tcp-client") | Some("tcp-server") => Protocol::Tcp,
        _ => Protocol::Udp,
    }
}

fn pem(value: &str, label: &str) -> String {
    let value = value.trim();
    let begin = format!("-----BEGIN {label}-----");
    let end = format!("-----END {label}-----");
    if value.starts_with(&begin) && value.ends_with(&end) {
        return format!("{value}\n");
    }
    value.to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn embedded_accounts_are_valid() {
        let accounts = embedded();
        assert!(accounts.len() > 100, "expected embedded accounts");
        for (host, account) in &accounts {
            let profile = account.profile(host).expect("profile");
            assert!(!profile.servers.is_empty());
            assert!(profile.credentials.ca.contains("BEGIN CERTIFICATE"));
            assert!(profile.credentials.cert.contains("BEGIN CERTIFICATE"));
            assert!(profile.credentials.key.contains("BEGIN PRIVATE KEY"));
            assert_eq!(profile.servers[0].host, *host);
            assert!(
                profile
                    .servers
                    .windows(2)
                    .all(|pair| pair[0].protocol == Protocol::Udp
                        || pair[1].protocol == Protocol::Tcp)
            );
        }
    }

    #[test]
    fn region_comes_from_toml() {
        let accounts = embedded();
        let account = accounts.values().next().expect("account");
        assert!(!account.region.is_empty());
        assert!(account.region.chars().all(|c| c.is_ascii_uppercase()));
    }

    #[test]
    fn parses_minimal_account() {
        let text = r#"
region = "SG"
cipher = "AES-128-CBC"
auth = "SHA256"
ca = "-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----"
cert = "-----BEGIN CERTIFICATE-----\nBBB\n-----END CERTIFICATE-----"
key = "-----BEGIN PRIVATE KEY-----\nCCC\n-----END PRIVATE KEY-----"

[[remotes]]
port = 1194
protocol = "tcp"
"#;
        let account = parse(text).expect("account");
        let profile = account.profile("198.51.100.1").expect("profile");
        assert_eq!(profile.cipher, Cipher::Aes128Cbc);
        assert_eq!(profile.auth, Auth::Sha256);
        assert_eq!(profile.servers[0].protocol, Protocol::Tcp);
        assert_eq!(profile.servers[0].port, 1194);
        assert!(profile.credentials.cert.starts_with("-----BEGIN"));
    }

    #[test]
    fn rejects_unknown_cipher() {
        let account = Account {
            region: "ID".into(),
            cipher: Some("ROT13".into()),
            auth: None,
            ca: String::new(),
            cert: String::new(),
            key: String::new(),
            remotes: Vec::new(),
        };
        assert!(account.profile("198.51.100.1").is_err());
    }
}
