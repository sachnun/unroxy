//! Psiphon tunnel core embedded through a cgo static archive.
//!
//! The tunnel stays in Go. Rust reaches it through the SOCKS5 listener the Go
//! side runs per tunnel, which keeps TLS and HTTP under Rust's control while
//! Go still reports which server carried each connection.

use std::ffi::{CStr, CString, c_char, c_int};
use std::io;
use std::path::Path;

use serde::{Deserialize, Serialize};

mod sys {
    use std::ffi::{c_char, c_int};

    unsafe extern "C" {
        pub fn PsiStart(config: *const c_char, server_entries: *const c_char) -> c_int;
        pub fn PsiState(handle: c_int) -> *mut c_char;
        pub fn PsiExitFor(handle: c_int, host: *const c_char) -> *mut c_char;
        pub fn PsiTerminate(handle: c_int, count: c_int);
        pub fn PsiStop(handle: c_int);
        pub fn PsiLastError() -> *mut c_char;
        pub fn PsiFree(ptr: *mut c_char);
    }
}

pub const SERVER_ENTRY_SIGNATURE_PUBLIC_KEY: &str = "sHuUVTWaRyh5pZwy4UguSgkwmBe0EHtJJkoF5WrxmvA=";

const READY_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(300);

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("psiphon: {0}")]
    Core(String),
    #[error("psiphon config: {0}")]
    Config(#[from] serde_json::Error),
    #[error("psiphon address: {0}")]
    Address(#[from] std::ffi::NulError),
    #[error("psiphon socket: {0}")]
    Socket(#[from] io::Error),
    #[error("psiphon tunnel not ready after {}s", READY_TIMEOUT.as_secs())]
    Timeout,
}

fn last_error() -> String {
    unsafe {
        let ptr = sys::PsiLastError();
        if ptr.is_null() {
            return String::new();
        }
        let message = CStr::from_ptr(ptr).to_string_lossy().into_owned();
        sys::PsiFree(ptr);
        message
    }
}

fn take_string(ptr: *mut c_char) -> String {
    unsafe {
        if ptr.is_null() {
            return String::new();
        }
        let value = CStr::from_ptr(ptr).to_string_lossy().into_owned();
        sys::PsiFree(ptr);
        value
    }
}

/// The config the Go core expects, mirroring `internal/core/config.go`.
#[derive(Debug, Clone, Deserialize)]
#[serde(default)]
pub struct Config {
    pub data_root_directory: String,
    pub egress_region: String,
    pub tunnel_pool_size: u32,
    pub establish_timeout_seconds: u32,
    pub client_platform: String,
    pub network_id: String,
    pub disable_dsl_fetcher: bool,
    pub limit_relay_buffer_sizes: bool,
    pub limit_cpu_threads: bool,
    pub connection_worker_pool_size: u32,
    pub connection_worker_pool_max_size: u32,
    pub limit_intensive_connection_workers: u32,
    pub ssh_channel_window_size: i32,
    pub disable_server_entries_reporter: bool,
    pub disable_replay: bool,
    pub ignore_handshake_stats_regexps: bool,
    pub disable_tactics: bool,
    pub emit_diagnostic_notices: bool,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            data_root_directory: String::new(),
            egress_region: String::new(),
            tunnel_pool_size: 1,
            establish_timeout_seconds: 300,
            client_platform: "Windows".to_string(),
            network_id: "WIFI".to_string(),
            disable_dsl_fetcher: true,
            limit_relay_buffer_sizes: true,
            limit_cpu_threads: true,
            connection_worker_pool_size: 2,
            connection_worker_pool_max_size: 2,
            limit_intensive_connection_workers: 1,
            ssh_channel_window_size: 32,
            disable_server_entries_reporter: true,
            disable_replay: true,
            ignore_handshake_stats_regexps: true,
            disable_tactics: true,
            emit_diagnostic_notices: true,
        }
    }
}

impl Config {
    fn to_json(&self) -> Result<CString, Error> {
        let mut map = serde_json::Map::new();
        map.insert("LocalSocksProxyPort".into(), 0.into());
        map.insert("LocalHttpProxyPort".into(), 0.into());
        map.insert("PropagationChannelId".into(), "FFFFFFFFFFFFFFFF".into());
        map.insert("SponsorId".into(), "FFFFFFFFFFFFFFFF".into());
        map.insert(
            "EstablishTunnelTimeoutSeconds".into(),
            self.establish_timeout_seconds.into(),
        );
        map.insert("TunnelPoolSize".into(), self.tunnel_pool_size.into());
        map.insert("DisableDSLFetcher".into(), self.disable_dsl_fetcher.into());
        map.insert(
            "DataRootDirectory".into(),
            self.data_root_directory.clone().into(),
        );
        map.insert("NetworkID".into(), self.network_id.clone().into());
        map.insert(
            "EmitDiagnosticNotices".into(),
            self.emit_diagnostic_notices.into(),
        );
        map.insert("DisableTactics".into(), self.disable_tactics.into());
        map.insert("LimitMeekBufferSizes".into(), false.into());
        map.insert(
            "LimitRelayBufferSizes".into(),
            self.limit_relay_buffer_sizes.into(),
        );
        map.insert("LimitCPUThreads".into(), self.limit_cpu_threads.into());
        map.insert(
            "ConnectionWorkerPoolSize".into(),
            self.connection_worker_pool_size.into(),
        );
        map.insert(
            "ConnectionWorkerPoolMaxSize".into(),
            self.connection_worker_pool_max_size.into(),
        );
        map.insert(
            "LimitIntensiveConnectionWorkers".into(),
            self.limit_intensive_connection_workers.into(),
        );
        map.insert(
            "SSHChannelWindowSize".into(),
            self.ssh_channel_window_size.into(),
        );
        map.insert(
            "DisableServerEntriesReporter".into(),
            self.disable_server_entries_reporter.into(),
        );
        map.insert("DisableReplay".into(), self.disable_replay.into());
        map.insert(
            "IgnoreHandshakeStatsRegexps".into(),
            self.ignore_handshake_stats_regexps.into(),
        );
        map.insert(
            "ServerEntrySignaturePublicKey".into(),
            SERVER_ENTRY_SIGNATURE_PUBLIC_KEY.into(),
        );
        if !self.egress_region.is_empty() {
            map.insert("EgressRegion".into(), self.egress_region.clone().into());
        }
        if !self.client_platform.is_empty() {
            map.insert("ClientPlatform".into(), self.client_platform.clone().into());
        }
        Ok(CString::new(serde_json::Value::Object(map).to_string())?)
    }
}

/// Counters reported by the embedded controller.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct State {
    #[serde(default)]
    pub active: u32,
    #[serde(default)]
    pub connected: u32,
    #[serde(default)]
    pub socks_port: u16,
    #[serde(default)]
    pub notices: std::collections::HashMap<String, u64>,
}

/// The exit that served a host, as reported by the tunnel.
#[derive(Debug, Clone, Default, Deserialize, Serialize)]
pub struct Exit {
    #[serde(default)]
    pub ip: String,
    #[serde(default)]
    pub region: String,
    #[serde(default)]
    pub protocol: String,
}

impl Exit {
    pub fn is_empty(&self) -> bool {
        self.ip.is_empty()
    }
}

pub struct Tunnel {
    handle: c_int,
    target_pool: usize,
    region: String,
    socks_port: std::sync::atomic::AtomicU16,
    active: std::sync::atomic::AtomicUsize,
    connected: std::sync::atomic::AtomicUsize,
}

impl Tunnel {
    pub fn start(config: &Config, server_entries: &str, data_dir: &Path) -> Result<Self, Error> {
        std::fs::create_dir_all(data_dir)?;

        let config_json = config.to_json()?;
        let entries = CString::new(server_entries)?;
        let handle = unsafe { sys::PsiStart(config_json.as_ptr(), entries.as_ptr()) };
        if handle == 0 {
            return Err(Error::Core(last_error()));
        }

        Ok(Self {
            handle,
            target_pool: config.tunnel_pool_size as usize,
            region: config.egress_region.clone(),
            socks_port: std::sync::atomic::AtomicU16::new(0),
            active: std::sync::atomic::AtomicUsize::new(0),
            connected: std::sync::atomic::AtomicUsize::new(0),
        })
    }

    pub fn region(&self) -> &str {
        &self.region
    }

    pub fn target_pool(&self) -> usize {
        self.target_pool
    }

    /// Refreshes the cached counters. Called before a read that needs them to
    /// be current, so the cgo boundary is crossed once per refresh rather than
    /// once per read.
    pub fn refresh(&self) {
        let raw = take_string(unsafe { sys::PsiState(self.handle) });
        let Ok(state) = serde_json::from_str::<State>(&raw) else {
            return;
        };
        self.socks_port
            .store(state.socks_port, std::sync::atomic::Ordering::Relaxed);
        self.active
            .store(state.active as usize, std::sync::atomic::Ordering::Relaxed);
        self.connected.store(
            state.connected as usize,
            std::sync::atomic::Ordering::Relaxed,
        );
    }

    /// The counters as of the last refresh.
    pub fn state(&self) -> State {
        State {
            active: self.active.load(std::sync::atomic::Ordering::Relaxed) as u32,
            connected: self.connected.load(std::sync::atomic::Ordering::Relaxed) as u32,
            socks_port: self.socks_port.load(std::sync::atomic::Ordering::Relaxed),
            notices: Default::default(),
        }
    }

    pub fn active_tunnels(&self) -> usize {
        self.active.load(std::sync::atomic::Ordering::Relaxed)
    }

    pub fn is_ready(&self) -> bool {
        self.active_tunnels() > 0
    }

    /// The SOCKS5 endpoint Rust dials to reach the tunnel. The port is fixed
    /// once the listener exists, so the first read refreshes and caches it.
    pub fn socks_addr(&self) -> String {
        if self.socks_port.load(std::sync::atomic::Ordering::Relaxed) == 0 {
            self.refresh();
        }
        format!(
            "127.0.0.1:{}",
            self.socks_port.load(std::sync::atomic::Ordering::Relaxed)
        )
    }

    /// The same endpoint in URL form, for HTTP clients that take one.
    pub fn proxy_url(&self) -> String {
        format!("socks5h://{}", self.socks_addr())
    }

    /// The exit that served the most recent connection to `host`.
    pub fn exit_for(&self, host: &str) -> Exit {
        let Ok(host) = CString::new(host) else {
            return Exit::default();
        };
        let raw = take_string(unsafe { sys::PsiExitFor(self.handle, host.as_ptr()) });
        serde_json::from_str(&raw).unwrap_or_default()
    }

    /// Retires up to `count` active tunnels so the controller replaces them.
    pub fn terminate(&self, count: usize) {
        unsafe { sys::PsiTerminate(self.handle, count as c_int) };
    }

    /// Waits until the tunnel is carrying traffic.
    pub async fn wait_ready(&self) -> Result<(), Error> {
        let deadline = tokio::time::Instant::now() + READY_TIMEOUT;
        loop {
            self.refresh();
            if self.is_ready() {
                return Ok(());
            }
            if tokio::time::Instant::now() >= deadline {
                return Err(Error::Timeout);
            }
            tokio::time::sleep(std::time::Duration::from_millis(500)).await;
        }
    }

    /// A tunnel that never connects, for tests that exercise the pool.
    pub fn stub(region: &str, target_pool: usize) -> std::sync::Arc<Self> {
        std::sync::Arc::new(Self {
            handle: -1,
            target_pool,
            region: region.to_string(),
            socks_port: std::sync::atomic::AtomicU16::new(0),
            active: std::sync::atomic::AtomicUsize::new(0),
            connected: std::sync::atomic::AtomicUsize::new(0),
        })
    }
}

impl Drop for Tunnel {
    fn drop(&mut self) {
        if self.handle >= 0 {
            unsafe { sys::PsiStop(self.handle) };
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn config_json(config: &Config) -> serde_json::Value {
        serde_json::from_str(config.to_json().unwrap().to_str().unwrap()).unwrap()
    }

    #[test]
    fn config_json_matches_source_fields() {
        let config = Config {
            egress_region: "SG".into(),
            data_root_directory: "/tmp/x".into(),
            tunnel_pool_size: 3,
            ..Config::default()
        };
        let json = config_json(&config);
        assert_eq!(json["EgressRegion"], "SG");
        assert_eq!(json["TunnelPoolSize"], 3);
        assert_eq!(json["DataRootDirectory"], "/tmp/x");
        assert_eq!(json["SponsorId"], "FFFFFFFFFFFFFFFF");
        assert_eq!(json["PropagationChannelId"], "FFFFFFFFFFFFFFFF");
        assert_eq!(json["EstablishTunnelTimeoutSeconds"], 300);
        assert_eq!(json["LimitMeekBufferSizes"], false);
        assert_eq!(json["SSHChannelWindowSize"], 32);
        assert_eq!(json["ClientPlatform"], "Windows");
        assert!(json.get("EmitDiagnosticNotices").is_some());
    }

    #[test]
    fn empty_region_is_omitted() {
        assert!(
            config_json(&Config::default())
                .get("EgressRegion")
                .is_none()
        );
    }
}
