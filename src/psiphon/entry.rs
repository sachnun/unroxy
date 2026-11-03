use std::collections::BTreeMap;
use std::sync::LazyLock;

use include_dir::{Dir, include_dir};
use serde::Deserialize;

#[derive(Debug, Clone)]
pub struct Entry {
    pub ip: String,
    pub region: String,
    pub ssh_port: u16,
    pub ossh_port: u16,
    pub ssh_username: String,
    pub ssh_password: String,
    pub ssh_host_key: String,
    pub ssh_obfuscated_key: String,
}

#[derive(Deserialize)]
struct Account {
    region: String,
    ssh_port: u16,
    ossh_port: u16,
    ssh_username: String,
    ssh_password: String,
    ssh_host_key: String,
    ssh_obfuscated_key: String,
}

static ACCOUNTS: Dir<'static> = include_dir!("$CARGO_MANIFEST_DIR/accounts/psiphon");

static ENTRIES: LazyLock<Vec<Entry>> = LazyLock::new(|| load(&ACCOUNTS));

fn load(dir: &Dir<'_>) -> Vec<Entry> {
    let mut entries = Vec::new();
    for file in dir.files() {
        let Some(text) = file.contents_utf8() else {
            continue;
        };
        let account: Account = match toml::from_str(text) {
            Ok(account) => account,
            Err(err) => {
                tracing::warn!("psiphon account {}: {err}", file.path().display());
                continue;
            }
        };
        let Some(ip) = file
            .path()
            .file_stem()
            .and_then(|stem| stem.to_str())
            .map(str::to_string)
        else {
            continue;
        };
        entries.push(Entry {
            ip,
            region: account.region,
            ssh_port: account.ssh_port,
            ossh_port: account.ossh_port,
            ssh_username: account.ssh_username,
            ssh_password: account.ssh_password,
            ssh_host_key: account.ssh_host_key,
            ssh_obfuscated_key: account.ssh_obfuscated_key,
        });
    }
    entries
}

pub fn all() -> &'static [Entry] {
    &ENTRIES
}

pub fn parse(entries: &[Entry], region: &str) -> Vec<Entry> {
    entries
        .iter()
        .filter(|entry| region.is_empty() || entry.region == region)
        .cloned()
        .collect()
}

pub fn group_by_region(entries: &[Entry]) -> BTreeMap<String, Vec<Entry>> {
    let mut grouped: BTreeMap<String, Vec<Entry>> = BTreeMap::new();
    for entry in entries {
        if entry.region.is_empty() {
            continue;
        }
        grouped
            .entry(entry.region.clone())
            .or_default()
            .push(entry.clone());
    }
    grouped
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn loads_embedded_accounts() {
        let entries = all();
        assert!(entries.len() > 300, "expected embedded entries");
        assert!(entries.iter().all(|entry| !entry.ssh_host_key.is_empty()));
        assert!(entries.iter().all(|entry| !entry.region.is_empty()));
    }

    #[test]
    fn filters_entries_for_region() {
        let entries = all();
        let region = entries[0].region.clone();
        let parsed = parse(entries, &region);
        assert!(!parsed.is_empty());
        assert!(parsed.iter().all(|entry| entry.region == region));
        assert_eq!(parse(entries, "").len(), entries.len());
    }

    #[test]
    fn groups_entries_by_region() {
        let grouped = group_by_region(all());
        let total: usize = grouped.values().map(Vec::len).sum();
        assert_eq!(total, all().len());
        assert!(grouped.len() > 10);
    }
}
