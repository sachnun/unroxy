use std::collections::BTreeMap;
use std::sync::LazyLock;

use include_dir::{Dir, include_dir};
use serde::Deserialize;

#[derive(Debug, Clone)]
pub struct Account {
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
struct File {
    region: String,
    ssh_port: u16,
    ossh_port: u16,
    ssh_username: String,
    ssh_password: String,
    ssh_host_key: String,
    ssh_obfuscated_key: String,
}

static FILES: Dir<'static> = include_dir!("$CARGO_MANIFEST_DIR/accounts/ssh");

static ACCOUNTS: LazyLock<Vec<Account>> = LazyLock::new(|| load(&FILES));

fn load(dir: &Dir<'_>) -> Vec<Account> {
    let mut accounts = Vec::new();
    for file in dir.files() {
        let Some(text) = file.contents_utf8() else {
            continue;
        };
        let account: File = match toml::from_str(text) {
            Ok(account) => account,
            Err(err) => {
                tracing::warn!("ssh account {}: {err}", file.path().display());
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
        accounts.push(Account {
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
    accounts
}

pub fn all() -> &'static [Account] {
    &ACCOUNTS
}

pub fn parse(accounts: &[Account], region: &str) -> Vec<Account> {
    accounts
        .iter()
        .filter(|account| region.is_empty() || account.region == region)
        .cloned()
        .collect()
}

pub fn group_by_region(accounts: &[Account]) -> BTreeMap<String, Vec<Account>> {
    let mut grouped: BTreeMap<String, Vec<Account>> = BTreeMap::new();
    for account in accounts {
        if account.region.is_empty() {
            continue;
        }
        grouped
            .entry(account.region.clone())
            .or_default()
            .push(account.clone());
    }
    grouped
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn loads_embedded_accounts() {
        let accounts = all();
        assert!(accounts.len() > 300, "expected embedded accounts");
        assert!(
            accounts
                .iter()
                .all(|account| !account.ssh_host_key.is_empty())
        );
        assert!(accounts.iter().all(|account| !account.region.is_empty()));
    }

    #[test]
    fn filters_accounts_for_region() {
        let accounts = all();
        let region = accounts[0].region.clone();
        let parsed = parse(accounts, &region);
        assert!(!parsed.is_empty());
        assert!(parsed.iter().all(|account| account.region == region));
        assert_eq!(parse(accounts, "").len(), accounts.len());
    }

    #[test]
    fn groups_accounts_by_region() {
        let grouped = group_by_region(all());
        let total: usize = grouped.values().map(Vec::len).sum();
        assert_eq!(total, all().len());
        assert!(grouped.len() > 10);
    }
}
