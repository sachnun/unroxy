use std::collections::BTreeMap;

use serde::Deserialize;

#[derive(Debug, Clone, Deserialize)]
pub struct Entry {
    #[serde(rename = "ipAddress", default)]
    pub ip: String,
    #[serde(rename = "sshPort", default)]
    pub ssh_port: u16,
    #[serde(rename = "sshObfuscatedPort", default)]
    pub ossh_port: u16,
    #[serde(rename = "sshUsername", default)]
    pub ssh_username: String,
    #[serde(rename = "sshPassword", default)]
    pub ssh_password: String,
    #[serde(rename = "sshHostKey", default)]
    pub ssh_host_key: String,
    #[serde(rename = "sshObfuscatedKey", default)]
    pub ssh_obfuscated_key: String,
    #[serde(default)]
    pub region: String,
}

pub fn parse(entries: &str, region: &str) -> Vec<Entry> {
    parse_filtered(entries, Some(region))
}

pub fn regions(entries: &str) -> Vec<String> {
    group_by_region(entries).into_keys().collect()
}

pub fn group_by_region(entries: &str) -> BTreeMap<String, Vec<String>> {
    let mut grouped: BTreeMap<String, Vec<String>> = BTreeMap::new();
    for line in entries.lines() {
        let line = line.trim();
        if line.is_empty() {
            continue;
        }
        let Some(entry) = parse_line(line) else {
            continue;
        };
        if entry.region.is_empty() {
            continue;
        }
        grouped
            .entry(entry.region)
            .or_default()
            .push(line.to_string());
    }
    grouped
}

fn parse_filtered(entries: &str, region: Option<&str>) -> Vec<Entry> {
    let mut parsed = Vec::new();
    for line in entries.lines() {
        let line = line.trim();
        if line.is_empty() {
            continue;
        }
        let Some(entry) = parse_line(line) else {
            continue;
        };
        if let Some(region) = region
            && !region.is_empty()
            && entry.region != region
        {
            continue;
        }
        parsed.push(entry);
    }
    parsed
}

fn parse_line(line: &str) -> Option<Entry> {
    let decoded = hex::decode(line).ok()?;
    let text = String::from_utf8_lossy(&decoded);
    let start = text.find('{')?;
    let entry: Entry = serde_json::from_str(&text[start..]).ok()?;
    if entry.ip.is_empty() || entry.ssh_username.is_empty() || entry.ssh_host_key.is_empty() {
        return None;
    }
    Some(entry)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn line(ip: &str, region: &str) -> String {
        let json = serde_json::json!({
            "ipAddress": ip,
            "sshPort": 22,
            "sshObfuscatedPort": 554,
            "sshUsername": "user",
            "sshPassword": "pass",
            "sshHostKey": "key",
            "sshObfuscatedKey": "obf",
            "region": region,
        });
        hex::encode(json.to_string())
    }

    #[test]
    fn parses_entries_for_region() {
        let text = format!(
            "{}\n{}\n",
            line("203.0.113.9", "US"),
            line("198.51.100.4", "GB")
        );
        let parsed = parse(&text, "US");
        assert_eq!(parsed.len(), 1);
        assert_eq!(parsed[0].ip, "203.0.113.9");
        assert_eq!(parsed[0].ossh_port, 554);
    }

    #[test]
    fn skips_malformed_and_other_regions() {
        let text = format!("zzzz\n{}\n", line("203.0.113.9", "GB"));
        assert!(parse(&text, "US").is_empty());
    }
}
