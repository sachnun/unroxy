use std::net::SocketAddr;
use std::path::PathBuf;
use std::process::Stdio;
use std::sync::Arc;
use std::time::Duration;

use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};

use unroxy_proxy as front;
use unroxy_proxy::{PoolProxy, ProxyPool};

static MEASURE: tokio::sync::Mutex<()> = tokio::sync::Mutex::const_new(());
const FULL_POOL_SESSIONS: u32 = 427;

fn cpu_ms() -> f64 {
    let mut ts = libc::timespec {
        tv_sec: 0,
        tv_nsec: 0,
    };
    unsafe { libc::clock_gettime(libc::CLOCK_PROCESS_CPUTIME_ID, &mut ts) };
    ts.tv_sec as f64 * 1000.0 + ts.tv_nsec as f64 / 1e6
}

fn rss_kb() -> u64 {
    let status = std::fs::read_to_string("/proc/self/status").unwrap_or_default();
    for line in status.lines() {
        if let Some(rest) = line.strip_prefix("VmRSS:") {
            return rest
                .split_whitespace()
                .next()
                .and_then(|v| v.parse().ok())
                .unwrap_or(0);
        }
    }
    0
}

fn established(port: u16) -> usize {
    let text = std::fs::read_to_string("/proc/net/tcp").unwrap_or_default();
    text.lines()
        .skip(1)
        .filter(|line| {
            let fields: Vec<&str> = line.split_whitespace().collect();
            if fields.len() < 4 {
                return false;
            }
            let local_port = fields[1]
                .split(':')
                .nth(1)
                .and_then(|v| u16::from_str_radix(v, 16).ok());
            local_port == Some(port) && fields[3] == "01"
        })
        .count()
}

async fn settle() {
    tokio::time::sleep(Duration::from_millis(200)).await;
}

fn mock_server_bin() -> Option<PathBuf> {
    let exe = std::env::current_exe().ok()?;
    let dir = exe.parent()?.parent()?;
    let path = dir.join("mock_ssh_server");
    path.exists().then_some(path)
}

async fn spawn_ssh_server() -> Option<(tokio::process::Child, u16, String)> {
    let path = mock_server_bin()?;
    let mut child = tokio::process::Command::new(path)
        .stdout(Stdio::piped())
        .kill_on_drop(true)
        .spawn()
        .ok()?;
    let stdout = child.stdout.take()?;
    let mut stdout = tokio::io::BufReader::new(stdout);
    let mut line = String::new();
    stdout.read_line(&mut line).await.ok()?;
    let mut parts = line.split_whitespace();
    let port: u16 = parts.next()?.parse().ok()?;
    let key = parts.next()?.to_string();
    Some((child, port, key))
}

fn build_ssh_tunnel(
    port: u16,
    host_key: &str,
    pool: u32,
) -> std::io::Result<Arc<unroxy_psiphon::Tunnel>> {
    let entry = serde_json::json!({
        "ipAddress": "127.0.0.1",
        "sshPort": port,
        "sshUsername": "unroxy",
        "sshPassword": "unroxy",
        "sshHostKey": host_key,
        "region": "MOCK",
    })
    .to_string();
    let entries = format!("{}\n", hex::encode(entry));
    let config = unroxy_psiphon::Config {
        data_root_directory: "/tmp/unroxy-mock-ssh".to_string(),
        egress_region: "MOCK".to_string(),
        tunnel_pool_size: pool,
        ..Default::default()
    };
    let dir = std::path::Path::new(&config.data_root_directory);
    unroxy_psiphon::Tunnel::start(&config, &entries, dir)
        .map(Arc::new)
        .map_err(std::io::Error::other)
}

fn proxy_with(tunnel: Arc<unroxy_psiphon::Tunnel>) -> Arc<front::Proxy> {
    let pool = ProxyPool::new(vec![PoolProxy {
        key: "psiphon://test".into(),
        tunnel,
        priority: 0,
    }]);
    front::Proxy::new(pool, None)
}

async fn spawn_proxy(proxy: Arc<front::Proxy>) -> SocketAddr {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(Arc::new(front::Server::new(proxy)).serve(listener));
    addr
}

async fn open_tunnel(proxy: SocketAddr, target: SocketAddr) -> std::io::Result<TcpStream> {
    let mut stream = TcpStream::connect(proxy).await?;
    let request = format!("CONNECT {target} HTTP/1.1\r\nHost: {target}\r\n\r\n");
    stream.write_all(request.as_bytes()).await?;
    let mut seen = Vec::new();
    let mut tmp = [0u8; 512];
    loop {
        let n = stream.read(&mut tmp).await?;
        if n == 0 {
            return Err(std::io::Error::other("proxy closed before CONNECT reply"));
        }
        seen.extend_from_slice(&tmp[..n]);
        if seen.windows(4).any(|w| w == b"\r\n\r\n") {
            return Ok(stream);
        }
    }
}

async fn hold_tunnel(proxy: SocketAddr, target: SocketAddr) {
    let Ok(mut stream) = open_tunnel(proxy, target).await else {
        return;
    };
    stream
        .write_all(b"GET / HTTP/1.1\r\nHost: origin\r\n\r\n")
        .await
        .ok();
    let mut buf = [0u8; 4096];
    loop {
        match stream.read(&mut buf).await {
            Ok(0) | Err(_) => return,
            Ok(_) => {}
        }
    }
}

async fn load_tunnels(
    proxy: SocketAddr,
    target: SocketAddr,
    connections: usize,
    steps: usize,
) -> u64 {
    let warmup = tokio::spawn(hold_tunnel(proxy, target));
    settle().await;
    let idle = rss_kb();
    warmup.abort();
    settle().await;

    let mut handles = Vec::with_capacity(connections);
    let cpu_start = cpu_ms();
    for _ in 0..connections {
        handles.push(tokio::spawn(hold_tunnel(proxy, target)));
    }
    for _ in 0..steps {
        tokio::time::sleep(Duration::from_millis(100)).await;
        if established(proxy.port()) >= connections {
            break;
        }
    }
    settle().await;
    let live = established(proxy.port());
    let loaded = rss_kb();
    let cpu_loaded = cpu_ms();

    tokio::time::sleep(Duration::from_secs(5)).await;
    let cpu_idle_window = cpu_ms();

    for handle in handles {
        handle.abort();
    }
    tokio::time::sleep(Duration::from_secs(2)).await;

    let open_cpu = cpu_loaded - cpu_start;
    let idle_cpu_per_s = (cpu_idle_window - cpu_loaded) / 5.0;
    println!(
        "connections={connections} idle_kb={idle} loaded_kb={loaded} established={live} per_conn_kb={} open_cpu_ms={open_cpu:.0} open_cpu_us_per_conn={:.0} idle_cpu_ms_per_s={idle_cpu_per_s:.1}",
        loaded.saturating_sub(idle) / connections as u64,
        open_cpu * 1000.0 / connections as f64,
    );
    loaded.saturating_sub(idle)
}

#[tokio::test(flavor = "multi_thread", worker_threads = 8)]
#[ignore = "needs mock_ssh_server built; run with --ignored"]
async fn ram_ssh_concurrent_100() {
    let _guard = MEASURE.lock().await;
    let Some((_server, port, key)) = spawn_ssh_server().await else {
        eprintln!("mock_ssh_server not built; run: cargo build --release --bin mock_ssh_server");
        return;
    };
    let tunnel = build_ssh_tunnel(port, &key, 4).unwrap();
    tokio::time::timeout(Duration::from_secs(60), tunnel.wait_ready())
        .await
        .expect("tunnel ready")
        .expect("tunnel ready");
    let proxy = spawn_proxy(proxy_with(tunnel)).await;
    let target: SocketAddr = "127.0.0.1:1".parse().unwrap();

    let delta = load_tunnels(proxy, target, 100, 40).await;
    assert!(delta < 64 * 1024);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 8)]
#[ignore = "needs mock_ssh_server built; run with --ignored"]
async fn ram_ssh_concurrent_5000() {
    let _guard = MEASURE.lock().await;
    let Some((_server, port, key)) = spawn_ssh_server().await else {
        eprintln!("mock_ssh_server not built; run: cargo build --release --bin mock_ssh_server");
        return;
    };
    let tunnel = build_ssh_tunnel(port, &key, 4).unwrap();
    tokio::time::timeout(Duration::from_secs(60), tunnel.wait_ready())
        .await
        .expect("tunnel ready")
        .expect("tunnel ready");
    let proxy = spawn_proxy(proxy_with(tunnel)).await;
    let target: SocketAddr = "127.0.0.1:1".parse().unwrap();

    let delta = load_tunnels(proxy, target, 5000, 180).await;
    assert!(delta < 512 * 1024, "5000 tunnels grew RSS by {delta} kB");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 8)]
#[ignore = "needs mock_ssh_server built; run with --ignored"]
async fn ram_ssh_sessions() {
    let _guard = MEASURE.lock().await;
    let Some((_server, port, key)) = spawn_ssh_server().await else {
        eprintln!("mock_ssh_server not built");
        return;
    };
    let base = rss_kb();
    let target: u32 = FULL_POOL_SESSIONS;
    let cpu_start = cpu_ms();
    let started = std::time::Instant::now();
    let tunnel = build_ssh_tunnel(port, &key, target).unwrap();
    let mut active = 0;
    for _ in 0..900 {
        active = tunnel.active_tunnels();
        if active >= target as usize {
            break;
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
    let handshake = started.elapsed();
    let cpu_handshake = cpu_ms() - cpu_start;
    tokio::time::sleep(Duration::from_secs(10)).await;
    let cpu_idle_window = cpu_ms();
    let loaded = rss_kb();
    println!(
        "sessions target={target} active={active} rss_kb base={base} loaded={loaded} per_session_kb={} handshake_s={:.1} handshake_cpu_ms={:.0} per_session_cpu_ms={:.2} idle_cpu_ms_per_s={:.3}",
        loaded.saturating_sub(base) / target as u64,
        handshake.as_secs_f64(),
        cpu_handshake,
        cpu_handshake / target as f64,
        (cpu_idle_window - cpu_start - cpu_handshake) / 10.0
    );
    assert!(active > 0);
}

async fn pump(proxy: SocketAddr, target: SocketAddr, bytes: usize) {
    let Ok(stream) = open_tunnel(proxy, target).await else {
        return;
    };
    let (mut reader, mut writer) = stream.into_split();
    let upload = tokio::spawn(async move {
        let chunk = vec![b'x'; 64 * 1024];
        let mut left = bytes;
        while left > 0 {
            let n = left.min(chunk.len());
            if writer.write_all(&chunk[..n]).await.is_err() {
                return;
            }
            left -= n;
        }
        let _ = writer.shutdown().await;
    });
    let mut got = 0;
    let mut buf = vec![0u8; 64 * 1024];
    while got < bytes {
        match reader.read(&mut buf).await {
            Ok(0) | Err(_) => break,
            Ok(n) => got += n,
        }
    }
    let _ = upload.await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 8)]
#[ignore = "needs mock_ssh_server built; run with --ignored"]
async fn cpu_throughput() {
    let _guard = MEASURE.lock().await;
    let Some((_server, port, key)) = spawn_ssh_server().await else {
        eprintln!("mock_ssh_server not built");
        return;
    };
    let tunnel = build_ssh_tunnel(port, &key, 4).unwrap();
    tokio::time::timeout(Duration::from_secs(60), tunnel.wait_ready())
        .await
        .expect("tunnel ready")
        .expect("tunnel ready");
    let proxy = spawn_proxy(proxy_with(tunnel)).await;
    let target: SocketAddr = "127.0.0.1:1".parse().unwrap();

    let streams = 32usize;
    let per = 2 * 1024 * 1024;
    let cpu_start = cpu_ms();
    let started = std::time::Instant::now();
    let mut handles = Vec::with_capacity(streams);
    for _ in 0..streams {
        handles.push(tokio::spawn(pump(proxy, target, per)));
    }
    for handle in handles {
        let _ = handle.await;
    }
    let cpu = cpu_ms() - cpu_start;
    let wall = started.elapsed().as_secs_f64();
    let mib = (streams * per) as f64 / (1024.0 * 1024.0);
    println!(
        "throughput payload_mib={mib:.0} wall_s={wall:.2} cpu_ms={cpu:.0} payload_mib_per_s={:.1} cpu_ms_per_mib={:.2}",
        mib / wall,
        cpu / mib
    );
}
