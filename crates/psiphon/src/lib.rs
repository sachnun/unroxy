mod entry;
mod ossh;
mod session;
mod socks;

use std::collections::HashMap;
use std::path::Path;
use std::sync::atomic::{AtomicU16, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex, RwLock};
use std::time::Duration;

use serde::{Deserialize, Serialize};

use entry::Entry;
use session::{Session, Timeouts};

pub const SERVER_ENTRY_SIGNATURE_PUBLIC_KEY: &str = "sHuUVTWaRyh5pZwy4UguSgkwmBe0EHtJJkoF5WrxmvA=";

const READY_TIMEOUT: Duration = Duration::from_secs(300);
const MANAGER_INTERVAL: Duration = Duration::from_secs(1);
const PICK_TIMEOUT: Duration = Duration::from_secs(10);

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("psiphon: {0}")]
    Core(String),
    #[error("psiphon socket: {0}")]
    Socket(#[from] std::io::Error),
    #[error("psiphon tunnel not ready")]
    Timeout,
    #[error("psiphon authentication rejected")]
    Auth,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(default)]
pub struct Config {
    pub data_root_directory: String,
    pub egress_region: String,
    pub tunnel_pool_size: u32,
    pub establish_timeout_seconds: u32,
    pub client_platform: String,
    pub sponsor_id: String,
    pub propagation_channel_id: String,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            data_root_directory: String::new(),
            egress_region: String::new(),
            tunnel_pool_size: 1,
            establish_timeout_seconds: 300,
            client_platform: "Windows".to_string(),
            sponsor_id: "FFFFFFFFFFFFFFFF".to_string(),
            propagation_channel_id: "FFFFFFFFFFFFFFFF".to_string(),
        }
    }
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Exit {
    pub ip: String,
    pub region: String,
    pub protocol: String,
}

impl Exit {
    pub fn is_empty(&self) -> bool {
        self.ip.is_empty()
    }
}

struct Inner {
    region: String,
    target_pool: usize,
    socks_port: AtomicU16,
    sessions: RwLock<Vec<Arc<Session>>>,
    exits: RwLock<HashMap<String, Exit>>,
    entries: Vec<Entry>,
    rotate: AtomicUsize,
    terminate_pending: AtomicUsize,
    connecting: AtomicUsize,
    active: AtomicUsize,
    timeouts: Timeouts,
    sponsor_id: String,
    propagation_channel_id: String,
    client_platform: String,
    tasks: Mutex<Vec<tokio::task::JoinHandle<()>>>,
}

impl Inner {
    async fn pick_session(&self) -> Option<Arc<Session>> {
        let deadline = tokio::time::Instant::now() + PICK_TIMEOUT;
        loop {
            {
                let sessions = self.sessions.read().expect("sessions lock");
                if !sessions.is_empty() {
                    let index = self.rotate.fetch_add(1, Ordering::Relaxed) % sessions.len();
                    return Some(Arc::clone(&sessions[index]));
                }
            }
            if tokio::time::Instant::now() >= deadline {
                return None;
            }
            tokio::time::sleep(Duration::from_millis(200)).await;
        }
    }

    fn record_exit(&self, host: &str, session: &Session) {
        let exit = Exit {
            ip: session.ip.clone(),
            region: self.region.clone(),
            protocol: session.protocol.to_string(),
        };
        self.exits
            .write()
            .expect("exits lock")
            .insert(host.to_lowercase(), exit);
    }

    fn live(&self) -> usize {
        self.sessions.read().expect("sessions lock").len()
    }
}

pub struct Tunnel {
    inner: Arc<Inner>,
}

impl Tunnel {
    pub fn start(config: &Config, server_entries: &str, data_dir: &Path) -> Result<Self, Error> {
        std::fs::create_dir_all(data_dir)?;
        let entries = entry::parse(server_entries, &config.egress_region);

        let listener = std::net::TcpListener::bind(("127.0.0.1", 0))?;
        listener.set_nonblocking(true)?;
        let port = listener.local_addr()?.port();
        let listener = tokio::net::TcpListener::from_std(listener)?;

        let inner = Arc::new(Inner {
            region: config.egress_region.clone(),
            target_pool: config.tunnel_pool_size.max(1) as usize,
            socks_port: AtomicU16::new(port),
            sessions: RwLock::new(Vec::new()),
            exits: RwLock::new(HashMap::new()),
            entries,
            rotate: AtomicUsize::new(0),
            terminate_pending: AtomicUsize::new(0),
            connecting: AtomicUsize::new(0),
            active: AtomicUsize::new(0),
            timeouts: Timeouts {
                connect: Duration::from_secs(10),
                handshake: Duration::from_secs(20),
                auth: Duration::from_secs(15),
            },
            sponsor_id: config.sponsor_id.clone(),
            propagation_channel_id: config.propagation_channel_id.clone(),
            client_platform: config.client_platform.clone(),
            tasks: Mutex::new(Vec::new()),
        });

        let accept = tokio::spawn(socks::serve(listener, Arc::clone(&inner)));
        let manager = tokio::spawn(manage(Arc::clone(&inner)));
        inner
            .tasks
            .lock()
            .expect("tasks lock")
            .extend([accept, manager]);

        Ok(Self { inner })
    }

    pub fn stub(region: &str, target_pool: usize) -> Arc<Self> {
        Arc::new(Self {
            inner: Arc::new(Inner {
                region: region.to_string(),
                target_pool,
                socks_port: AtomicU16::new(0),
                sessions: RwLock::new(Vec::new()),
                exits: RwLock::new(HashMap::new()),
                entries: Vec::new(),
                rotate: AtomicUsize::new(0),
                terminate_pending: AtomicUsize::new(0),
                connecting: AtomicUsize::new(0),
                active: AtomicUsize::new(0),
                timeouts: Timeouts {
                    connect: Duration::from_secs(10),
                    handshake: Duration::from_secs(20),
                    auth: Duration::from_secs(15),
                },
                sponsor_id: String::new(),
                propagation_channel_id: String::new(),
                client_platform: String::new(),
                tasks: Mutex::new(Vec::new()),
            }),
        })
    }

    pub fn region(&self) -> &str {
        &self.inner.region
    }

    pub fn target_pool(&self) -> usize {
        self.inner.target_pool
    }

    pub fn active_tunnels(&self) -> usize {
        self.inner.active.load(Ordering::Relaxed)
    }

    pub fn is_ready(&self) -> bool {
        self.active_tunnels() > 0
    }

    pub fn socks_addr(&self) -> String {
        format!(
            "127.0.0.1:{}",
            self.inner.socks_port.load(Ordering::Relaxed)
        )
    }

    pub fn proxy_url(&self) -> String {
        format!("socks5h://{}", self.socks_addr())
    }

    pub fn refresh(&self) {}

    pub fn terminate(&self, count: usize) {
        self.inner
            .terminate_pending
            .fetch_add(count, Ordering::Relaxed);
    }

    pub fn exit_for(&self, host: &str) -> Exit {
        self.inner
            .exits
            .read()
            .expect("exits lock")
            .get(&host.to_lowercase())
            .cloned()
            .unwrap_or_default()
    }

    pub async fn wait_ready(&self) -> Result<(), Error> {
        let deadline = tokio::time::Instant::now() + READY_TIMEOUT;
        while !self.is_ready() {
            if tokio::time::Instant::now() >= deadline {
                return Err(Error::Timeout);
            }
            tokio::time::sleep(Duration::from_millis(500)).await;
        }
        Ok(())
    }
}

impl Drop for Tunnel {
    fn drop(&mut self) {
        for task in self.inner.tasks.lock().expect("tasks lock").drain(..) {
            task.abort();
        }
    }
}

async fn manage(inner: Arc<Inner>) {
    loop {
        tokio::time::sleep(MANAGER_INTERVAL).await;
        reap(&inner).await;

        let retire = inner.terminate_pending.swap(0, Ordering::Relaxed);
        for _ in 0..retire {
            let session = inner.sessions.write().expect("sessions lock").pop();
            if let Some(session) = session {
                session.close().await;
            }
        }
        inner.active.store(inner.live(), Ordering::Relaxed);

        let need = inner
            .target_pool
            .saturating_sub(inner.live() + inner.connecting.load(Ordering::Relaxed));
        for _ in 0..need {
            if inner.entries.is_empty() {
                break;
            }
            let index = inner.rotate.fetch_add(1, Ordering::Relaxed) % inner.entries.len();
            let entry = inner.entries[index].clone();
            inner.connecting.fetch_add(1, Ordering::Relaxed);
            let inner = Arc::clone(&inner);
            tokio::spawn(async move {
                if let Ok(session) = Session::connect(
                    &entry,
                    &inner.timeouts,
                    &inner.sponsor_id,
                    &inner.propagation_channel_id,
                    &inner.client_platform,
                )
                .await
                {
                    inner.sessions.write().expect("sessions lock").push(session);
                }
                inner.connecting.fetch_sub(1, Ordering::Relaxed);
                inner.active.store(inner.live(), Ordering::Relaxed);
            });
        }
    }
}

async fn reap(inner: &Inner) {
    let snapshot: Vec<Arc<Session>> = inner.sessions.read().expect("sessions lock").clone();
    let mut dead = Vec::new();
    for session in &snapshot {
        if session.is_closed().await {
            dead.push(Arc::clone(session));
        }
    }
    if dead.is_empty() {
        return;
    }
    inner
        .sessions
        .write()
        .expect("sessions lock")
        .retain(|session| !dead.iter().any(|dead| Arc::ptr_eq(dead, session)));
    inner.active.store(inner.live(), Ordering::Relaxed);
}
