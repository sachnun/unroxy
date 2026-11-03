use std::path::Path;

use crate::openvpn::{Auth, Cipher, Credentials, Profile, Protocol, Server};
use base64::Engine;
use serde::Deserialize;

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("account: {0}")]
    Account(String),
    #[error("io: {0}")]
    Io(#[from] std::io::Error),
}

#[derive(Debug, Clone, Deserialize)]
pub struct Account {
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub protocol: Option<String>,
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
    pub host: String,
    #[serde(default)]
    pub port: Option<u16>,
    #[serde(default)]
    pub protocol: Option<String>,
}

impl Account {
    pub fn from_toml(path: &Path) -> Result<Self, Error> {
        let text = std::fs::read_to_string(path)?;
        let mut account: Self =
            toml::from_str(&text).map_err(|err| Error::Account(err.to_string()))?;
        if account.name.is_empty() {
            account.name = path
                .file_stem()
                .and_then(|stem| stem.to_str())
                .unwrap_or("account")
                .to_string();
        }
        Ok(account)
    }

    pub fn from_ovpn(path: &Path) -> Result<Self, Error> {
        let text = std::fs::read_to_string(path)?;
        parse_ovpn(&text, path)
    }

    pub fn profile(&self) -> Result<Profile, Error> {
        if self.remotes.is_empty() {
            return Err(Error::Account("no remote".into()));
        }
        let default_protocol = protocol_of(self.protocol.as_deref());
        let cipher = self
            .cipher
            .as_deref()
            .and_then(Cipher::parse)
            .or_else(|| Cipher::parse("AES-256-GCM"))
            .ok_or_else(|| Error::Account("unsupported cipher".into()))?;
        let auth = self
            .auth
            .as_deref()
            .and_then(Auth::parse)
            .unwrap_or(Auth::Sha1);
        let mut servers = Vec::with_capacity(self.remotes.len());
        for remote in &self.remotes {
            servers.push(Server {
                host: remote.host.clone(),
                port: remote.port.unwrap_or(1194),
                protocol: remote
                    .protocol
                    .as_deref()
                    .map(|value| protocol_of(Some(value)))
                    .unwrap_or(default_protocol),
            });
        }
        servers.sort_by_key(|server| server.protocol != Protocol::Udp);
        servers.dedup_by(|a, b| a.host == b.host && a.protocol == b.protocol);
        Ok(Profile {
            servers,
            cipher,
            auth,
            credentials: Credentials {
                ca: inline_or_file(&self.ca)?,
                cert: inline_or_file(&self.cert)?,
                key: inline_or_file(&self.key)?,
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

fn inline_or_file(value: &str) -> Result<String, Error> {
    let value = value.trim();
    if value.starts_with("-----BEGIN") {
        return Ok(value.to_string());
    }
    if value.is_empty() {
        return Err(Error::Account("empty credential".into()));
    }
    let bytes = std::fs::read(value)?;
    Ok(String::from_utf8_lossy(&bytes).into_owned())
}

pub fn parse_ovpn(text: &str, path: &Path) -> Result<Account, Error> {
    let mut servers: Vec<Server> = Vec::new();
    let mut ciphers = Vec::new();
    let mut auth = Auth::Sha1;
    let mut ca = None;
    let mut cert = None;
    let mut key = None;
    let mut capture: Option<(&str, String)> = None;
    let mut skip: Option<&str> = None;
    let mut pending = Protocol::Udp;

    for line in text.lines() {
        let trimmed = line.trim();

        if let Some((name, body)) = capture.as_mut() {
            if trimmed == format!("</{name}>") {
                let value = normalize_pem(body);
                match *name {
                    "ca" => ca = Some(value),
                    "cert" => cert = Some(value),
                    "key" => key = Some(value),
                    _ => {}
                }
                capture = None;
                continue;
            }
            if !trimmed.is_empty() {
                body.push_str(trimmed);
                body.push('\n');
            }
            continue;
        }

        if let Some(name) = skip {
            if trimmed == format!("</{name}>") {
                skip = None;
            }
            continue;
        }

        if let Some(name) = trimmed
            .strip_prefix('<')
            .and_then(|rest| rest.strip_suffix('>'))
            .filter(|name| !name.starts_with('/'))
        {
            match name {
                "ca" | "cert" | "key" => capture = Some((name, String::new())),
                "tls-auth" | "tls-crypt" | "tls-crypt-v2" | "secret" => skip = Some(name),
                _ => {}
            }
            continue;
        }
        if trimmed.starts_with('<') || trimmed.starts_with('#') || trimmed.is_empty() {
            continue;
        }

        let mut parts = trimmed.split_whitespace();
        match parts.next() {
            Some("proto") => {
                if let Some(value) = parts.next() {
                    pending = protocol_of(Some(value));
                }
            }
            Some("remote") => {
                let Some(host) = parts.next() else { continue };
                let port = parts
                    .next()
                    .and_then(|value| value.parse::<u16>().ok())
                    .unwrap_or(1194);
                servers.push(Server {
                    host: host.to_string(),
                    port,
                    protocol: pending,
                });
            }
            Some("auth") => {
                if let Some(value) = parts.next()
                    && let Some(parsed) = Auth::parse(value)
                {
                    auth = parsed;
                }
            }
            Some("cipher") | Some("data-ciphers") | Some("ncp-ciphers") => {
                if let Some(value) = parts.next() {
                    for name in value.split(':') {
                        if let Some(cipher) = Cipher::parse(name)
                            && !ciphers.contains(&cipher)
                        {
                            ciphers.push(cipher);
                        }
                    }
                }
            }
            _ => {}
        }
    }

    servers.sort_by_key(|server| server.protocol != Protocol::Udp);
    servers.dedup_by(|a, b| a.host == b.host && a.protocol == b.protocol);

    if servers.is_empty() {
        return Err(Error::Account("no remote".into()));
    }
    Ok(Account {
        name: region_of(path),
        protocol: Some(servers[0].protocol.as_str().to_string()),
        cipher: ciphers.first().map(ToString::to_string),
        auth: Some(auth.as_str().to_string()),
        ca: ca.ok_or_else(|| Error::Account("no ca".into()))?,
        cert: cert.ok_or_else(|| Error::Account("no cert".into()))?,
        key: key.ok_or_else(|| Error::Account("no key".into()))?,
        remotes: servers
            .into_iter()
            .map(|server| Remote {
                host: server.host,
                port: Some(server.port),
                protocol: Some(server.protocol.as_str().to_string()),
            })
            .collect(),
    })
}

pub fn region_of(path: &Path) -> String {
    let stem = path
        .file_name()
        .and_then(|name| name.to_str())
        .unwrap_or_default();
    if let Some(start) = stem.find('[')
        && let Some(end) = stem[start..].find(']')
    {
        let code = &stem[start + 1..start + end];
        if !code.is_empty() {
            return code.to_ascii_uppercase();
        }
    }
    "UN".to_string()
}

fn normalize_pem(raw: &str) -> String {
    if raw.contains("-----BEGIN ") {
        let mut out = String::with_capacity(raw.len());
        let mut inside = false;
        for line in raw.lines() {
            let line = line.trim();
            if line.starts_with("-----BEGIN ") {
                inside = true;
                out.push_str(line);
                out.push('\n');
                continue;
            }
            if !inside {
                continue;
            }
            if line.starts_with("-----END ") {
                out.push_str(line);
                out.push('\n');
                inside = false;
                continue;
            }
            if line.is_empty() || line.contains(':') || !line.is_ascii() {
                continue;
            }
            out.push_str(line);
            out.push('\n');
        }
        return out;
    }

    let mut body = String::new();
    let mut collecting = false;
    for line in raw.lines() {
        let line = line.trim();
        if line.starts_with("Serial Number:") || line.starts_with("Serial Number :") {
            collecting = true;
            continue;
        }
        if collecting && line.starts_with("Signature Algorithm:") {
            break;
        }
        if collecting {
            body.push_str(line);
        }
    }
    if body.is_empty() {
        return raw.trim().to_string();
    }
    let Some(der) = base64::engine::general_purpose::STANDARD
        .decode(body.as_bytes())
        .ok()
    else {
        return raw.trim().to_string();
    };
    let encoded = base64::engine::general_purpose::STANDARD.encode(der);
    wrap("CERTIFICATE", &encoded)
}

fn wrap(label: &str, encoded: &str) -> String {
    let mut out = String::with_capacity(encoded.len() + 64);
    out.push_str("-----BEGIN ");
    out.push_str(label);
    out.push_str("-----\n");
    for chunk in encoded.as_bytes().chunks(64) {
        out.push_str(std::str::from_utf8(chunk).unwrap_or_default());
        out.push('\n');
    }
    out.push_str("-----END ");
    out.push_str(label);
    out.push_str("-----\n");
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    const PROFILE: &str = r#"
proto tcp-client
remote 198.51.100.7 443
proto udp
remote 198.51.100.7 443
cipher AES-256-GCM
data-ciphers AES-256-GCM:AES-128-GCM:AES-256-CBC
auth SHA256
<ca>
-----BEGIN CERTIFICATE-----
AAA
-----END CERTIFICATE-----
</ca>
<cert>
Certificate:
    Data:
        Serial Number:
            2e:a2:a5:50
        Signature Algorithm: sha256WithRSAEncryption
-----BEGIN CERTIFICATE-----
BBB
-----END CERTIFICATE-----
</cert>
<key>
-----BEGIN PRIVATE KEY-----
CCC
-----END PRIVATE KEY-----
</key>
"#;

    #[test]
    fn parses_ovpn_profile() {
        let account = parse_ovpn(PROFILE, Path::new("[SG] Server.ovpn")).unwrap();
        assert_eq!(account.name, "SG");
        assert_eq!(account.remotes.len(), 2);
        assert_eq!(account.remotes[0].protocol.as_deref(), Some("udp"));
        assert_eq!(account.cipher.as_deref(), Some("AES-256-GCM"));
        let profile = account.profile().unwrap();
        assert_eq!(profile.servers[0].protocol, Protocol::Udp);
        assert_eq!(profile.servers[1].protocol, Protocol::Tcp);
        assert_eq!(profile.auth, Auth::Sha256);
        assert!(profile.credentials.cert.contains("BEGIN CERTIFICATE"));
        assert!(!profile.credentials.cert.contains("Serial Number"));
    }

    #[test]
    fn toml_roundtrip() {
        let text = r#"
name = "ID"
cipher = "AES-128-CBC"
auth = "SHA1"
ca = "-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----"
cert = "-----BEGIN CERTIFICATE-----\nBBB\n-----END CERTIFICATE-----"
key = "-----BEGIN PRIVATE KEY-----\nCCC\n-----END PRIVATE KEY-----"

[[remotes]]
host = "203.0.113.9"
port = 1194
protocol = "udp"
"#;
        let account: Account = toml::from_str(text).unwrap();
        let profile = account.profile().unwrap();
        assert_eq!(profile.servers.len(), 1);
        assert_eq!(profile.cipher, Cipher::Aes128Cbc);
        assert_eq!(profile.servers[0].host, "203.0.113.9");
    }

    #[test]
    fn pem_is_extracted_from_openssl_dump() {
        let text = r#"
remote 198.51.100.7 443
<ca>
-----BEGIN CERTIFICATE-----
AAA
-----END CERTIFICATE-----
</ca>
<cert>
Certificate:
    Data:
        Serial Number:
            2e:a2:a5:50
    Signature Algorithm: sha256WithRSAEncryption
-----BEGIN CERTIFICATE-----
BBB
-----END CERTIFICATE-----
</cert>
<key>
-----BEGIN PRIVATE KEY-----
CCC
-----END PRIVATE KEY-----
</key>
"#;
        let account = parse_ovpn(text, Path::new("[DE] a.ovpn")).expect("account");
        assert!(account.cert.starts_with("-----BEGIN CERTIFICATE-----"));
        assert!(
            account
                .cert
                .trim_end()
                .ends_with("-----END CERTIFICATE-----")
        );
        assert!(!account.cert.contains("Serial Number"));
        assert!(!account.cert.contains("Certificate:"));
        assert_eq!(account.cert.matches("BEGIN CERTIFICATE").count(), 1);
    }

    #[test]
    fn tls_auth_blocks_are_skipped() {
        let text = r#"
remote 198.51.100.7
<ca>
-----BEGIN CERTIFICATE-----
AAA
-----END CERTIFICATE-----
</ca>
<cert>
-----BEGIN CERTIFICATE-----
BBB
-----END CERTIFICATE-----
</cert>
<key>
-----BEGIN PRIVATE KEY-----
CCC
-----END PRIVATE KEY-----
</key>
<tls-auth>
-----BEGIN OpenVPN Static key V1-----
DDD
-----END OpenVPN Static key V1-----
</tls-auth>
<tls-crypt>
-----BEGIN OpenVPN Static key V1-----
EEE
-----END OpenVPN Static key V1-----
</tls-crypt>
"#;
        let account = parse_ovpn(text, Path::new("[SG] b.ovpn")).expect("account");
        assert!(!account.key.contains("DDD"));
        assert!(!account.key.contains("EEE"));
        assert_eq!(account.key.trim_end().lines().count(), 3);
    }

    #[test]
    fn prefers_udp_remote() {
        let text = r#"
proto tcp-client
remote 198.51.100.8 443
proto udp
remote 198.51.100.9 1194
<ca>
-----BEGIN CERTIFICATE-----
AAA
-----END CERTIFICATE-----
</ca>
<cert>
-----BEGIN CERTIFICATE-----
BBB
-----END CERTIFICATE-----
</cert>
<key>
-----BEGIN PRIVATE KEY-----
CCC
-----END PRIVATE KEY-----
</key>
"#;
        let account = parse_ovpn(text, Path::new("[US] c.ovpn")).expect("account");
        let profile = account.profile().expect("profile");
        assert_eq!(profile.servers[0].protocol, Protocol::Udp);
        assert_eq!(profile.servers[0].host, "198.51.100.9");
        assert_eq!(profile.servers[0].port, 1194);
    }

    #[test]
    fn defaults_port_and_protocol() {
        let text = r#"
remote 198.51.100.10
<ca>
-----BEGIN CERTIFICATE-----
AAA
-----END CERTIFICATE-----
</ca>
<cert>
-----BEGIN CERTIFICATE-----
BBB
-----END CERTIFICATE-----
</cert>
<key>
-----BEGIN PRIVATE KEY-----
CCC
-----END PRIVATE KEY-----
</key>
"#;
        let account = parse_ovpn(text, Path::new("plain.ovpn")).expect("account");
        let profile = account.profile().expect("profile");
        assert_eq!(profile.servers[0].port, 1194);
        assert_eq!(profile.servers[0].protocol, Protocol::Udp);
        assert_eq!(profile.cipher, Cipher::Aes256Gcm, "fallback cipher");
        assert_eq!(profile.auth, Auth::Sha1, "fallback auth");
        assert_eq!(account.name, "UN");
    }

    #[test]
    fn rejects_profile_without_remote() {
        let text = "<ca>\n-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----\n</ca>\n";
        assert!(parse_ovpn(text, Path::new("x.ovpn")).is_err());
    }

    #[test]
    fn rejects_profile_without_credentials() {
        let text = "remote 198.51.100.7\n";
        assert!(parse_ovpn(text, Path::new("x.ovpn")).is_err());
    }

    #[test]
    fn region_from_filename() {
        assert_eq!(region_of(Path::new("[ID] Server (1.2.3.4).ovpn")), "ID");
        assert_eq!(region_of(Path::new("plain.ovpn")), "UN");
    }
}
