//! Server entry decoding, ported from `internal/core/servers.go`.

use std::collections::HashMap;

use base64::Engine;
use hmac::{Hmac, Mac};
use sha2::Sha256;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ServerEntry {
    pub id: String,
    pub ip: String,
    pub region: String,
    pub raw: String,
}

fn generate_tag(ip_address: &str, web_server_secret: &str) -> String {
    let mut mac = Hmac::<Sha256>::new_from_slice(web_server_secret.as_bytes())
        .expect("hmac accepts any key length");
    mac.update(ip_address.as_bytes());
    base64::engine::general_purpose::STANDARD.encode(mac.finalize().into_bytes())
}

fn tag_to_diagnostic_id(tag: &str) -> String {
    if tag.len() < 8 {
        return "<unknown>".to_string();
    }
    tag[..8].to_string()
}

#[derive(serde::Deserialize)]
struct RawEntry {
    #[serde(rename = "ipAddress", default)]
    ip_address: String,
    #[serde(rename = "webServerSecret", default)]
    web_server_secret: String,
    #[serde(default)]
    tag: String,
    #[serde(default)]
    region: String,
}

/// Decodes one hex-encoded server entry into its diagnostic id, IP and region.
pub fn decode_entry(line: &str) -> Option<(String, String, String)> {
    let decoded = hex::decode(line).ok()?;
    let decoded = String::from_utf8_lossy(&decoded);
    let start = decoded.find('{')?;
    let entry: RawEntry = serde_json::from_str(&decoded[start..]).ok()?;
    if entry.ip_address.is_empty() {
        return None;
    }
    let tag = if entry.tag.is_empty() {
        generate_tag(&entry.ip_address, &entry.web_server_secret)
    } else {
        entry.tag
    };
    Some((tag_to_diagnostic_id(&tag), entry.ip_address, entry.region))
}

pub fn parse_entries_by_region(raw: &str) -> HashMap<String, Vec<ServerEntry>> {
    let mut by_region: HashMap<String, Vec<ServerEntry>> = HashMap::new();
    for line in raw.lines() {
        let line = line.trim();
        if line.is_empty() {
            continue;
        }
        let Some((id, ip, region)) = decode_entry(line) else {
            continue;
        };
        if region.is_empty() {
            continue;
        }
        by_region
            .entry(region.clone())
            .or_default()
            .push(ServerEntry {
                id,
                ip,
                region,
                raw: line.to_string(),
            });
    }
    by_region
}

#[cfg(test)]
mod tests {
    use super::*;

    fn entry_line(ip: &str, secret: &str, region: &str) -> String {
        let json = format!(
            r#"{{"ipAddress":{},"webServerSecret":{},"region":{}}}"#,
            serde_json::to_string(ip).unwrap(),
            serde_json::to_string(secret).unwrap(),
            serde_json::to_string(region).unwrap()
        );
        hex::encode(json)
    }

    #[test]
    fn decodes_a_real_line() {
        let line = entry_line("203.0.113.9", "c2VjcmV0", "US");
        let (id, ip, region) = decode_entry(&line).expect("decodes");
        assert_eq!(ip, "203.0.113.9");
        assert_eq!(region, "US");
        assert_eq!(
            id,
            tag_to_diagnostic_id(&generate_tag("203.0.113.9", "c2VjcmV0"))
        );
    }

    #[test]
    fn rejects_malformed_lines() {
        assert!(decode_entry("zzzz").is_none());
        assert!(decode_entry(&hex::encode("plain text")).is_none());
        assert!(decode_entry(&entry_line("", "c2VjcmV0", "US")).is_none());
    }

    #[test]
    fn groups_by_region_and_skips_empty_region() {
        let raw = format!(
            "{}\n{}\n{}\n",
            entry_line("203.0.113.9", "c2VjcmV0", "US"),
            entry_line("198.51.100.4", "b3RoZXI=", "US"),
            entry_line("192.0.2.1", "dGhpcmQ=", "")
        );
        let by_region = parse_entries_by_region(&raw);
        assert_eq!(by_region["US"].len(), 2);
        assert!(!by_region.contains_key(""));
    }
}
