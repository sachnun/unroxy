use std::collections::HashMap;
use std::future::Future;
use std::io;
use std::net::{SocketAddr, ToSocketAddrs};
use std::pin::Pin;
use std::sync::{Mutex, OnceLock};
use std::task::{Context, Poll, Waker};
use std::time::Duration;

use tokio::io::{AsyncRead, AsyncWrite, ReadBuf};
use wasip2::io::poll::Pollable;
use wasip2::io::streams::{InputStream, OutputStream, StreamError};
use wasip2::sockets::instance_network::instance_network;
use wasip2::sockets::network::{ErrorCode, Ipv4SocketAddress, Ipv6SocketAddress};
use wasip2::sockets::tcp::{IpAddressFamily, IpSocketAddress, ShutdownType, TcpSocket};
use wasip2::sockets::tcp_create_socket::create_tcp_socket;

const POLL_INTERVAL: Duration = Duration::from_millis(1);

struct Inner {
    pollables: HashMap<usize, Pollable>,
    wakers: HashMap<usize, Waker>,
    next: usize,
}

struct Reactor {
    inner: Mutex<Inner>,
}

impl Reactor {
    fn new() -> Self {
        Self {
            inner: Mutex::new(Inner {
                pollables: HashMap::new(),
                wakers: HashMap::new(),
                next: 0,
            }),
        }
    }

    fn insert(&self, pollable: Pollable) -> usize {
        let mut inner = self.inner.lock().unwrap();
        let key = inner.next;
        inner.next += 1;
        inner.pollables.insert(key, pollable);
        key
    }

    fn remove(&self, key: usize) {
        let mut inner = self.inner.lock().unwrap();
        inner.pollables.remove(&key);
        inner.wakers.remove(&key);
    }

    fn ready(&self, key: usize, waker: &Waker) -> bool {
        let mut inner = self.inner.lock().unwrap();
        let ready = inner
            .pollables
            .get(&key)
            .map(Pollable::ready)
            .unwrap_or(false);
        if ready {
            inner.wakers.remove(&key);
        } else {
            inner.wakers.insert(key, waker.clone());
        }
        ready
    }
}

static REACTOR: OnceLock<Reactor> = OnceLock::new();

fn reactor() -> &'static Reactor {
    REACTOR.get_or_init(Reactor::new)
}

pub fn init() {
    tokio::spawn(drive());
}

async fn drive() {
    loop {
        let ready: Vec<Waker> = {
            let inner = reactor().inner.lock().unwrap();
            inner
                .wakers
                .iter()
                .filter(|(key, _)| {
                    inner
                        .pollables
                        .get(key)
                        .map(Pollable::ready)
                        .unwrap_or(false)
                })
                .map(|(_, waker)| waker.clone())
                .collect()
        };
        for waker in ready {
            waker.wake();
        }
        tokio::time::sleep(POLL_INTERVAL).await;
    }
}

fn wait(pollable: Pollable) -> Wait {
    Wait {
        key: reactor().insert(pollable),
        done: false,
    }
}

struct Wait {
    key: usize,
    done: bool,
}

impl Future for Wait {
    type Output = ();

    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        if reactor().ready(self.key, cx.waker()) {
            self.done = true;
            Poll::Ready(())
        } else {
            Poll::Pending
        }
    }
}

impl Drop for Wait {
    fn drop(&mut self) {
        reactor().remove(self.key);
    }
}

struct Subscription {
    key: Option<usize>,
}

impl Subscription {
    fn new() -> Self {
        Self { key: None }
    }

    fn poll(&mut self, cx: &mut Context<'_>, subscribe: impl FnOnce() -> Pollable) -> Poll<()> {
        let key = *self
            .key
            .get_or_insert_with(|| reactor().insert(subscribe()));
        if reactor().ready(key, cx.waker()) {
            Poll::Ready(())
        } else {
            Poll::Pending
        }
    }
}

impl Drop for Subscription {
    fn drop(&mut self) {
        if let Some(key) = self.key.take() {
            reactor().remove(key);
        }
    }
}

fn to_io_err(err: ErrorCode) -> io::Error {
    let kind = match err {
        ErrorCode::AccessDenied => io::ErrorKind::PermissionDenied,
        ErrorCode::NotSupported => io::ErrorKind::Unsupported,
        ErrorCode::InvalidArgument => io::ErrorKind::InvalidInput,
        ErrorCode::OutOfMemory => io::ErrorKind::OutOfMemory,
        ErrorCode::Timeout => io::ErrorKind::TimedOut,
        ErrorCode::WouldBlock => io::ErrorKind::WouldBlock,
        ErrorCode::InvalidState => io::ErrorKind::InvalidData,
        ErrorCode::AddressInUse => io::ErrorKind::AddrInUse,
        ErrorCode::ConnectionRefused => io::ErrorKind::ConnectionRefused,
        ErrorCode::ConnectionReset => io::ErrorKind::ConnectionReset,
        ErrorCode::ConnectionAborted => io::ErrorKind::ConnectionAborted,
        ErrorCode::ConcurrencyConflict => io::ErrorKind::AlreadyExists,
        _ => io::ErrorKind::Other,
    };
    io::Error::new(kind, format!("{err:?}"))
}

fn stream_err(err: StreamError) -> io::Error {
    match err {
        StreamError::Closed => io::Error::from(io::ErrorKind::ConnectionReset),
        StreamError::LastOperationFailed(err) => io::Error::other(err.to_debug_string()),
    }
}

fn sockaddr_to_wasi(addr: SocketAddr) -> IpSocketAddress {
    match addr {
        SocketAddr::V4(addr) => {
            let ip = addr.ip().octets();
            IpSocketAddress::Ipv4(Ipv4SocketAddress {
                address: (ip[0], ip[1], ip[2], ip[3]),
                port: addr.port(),
            })
        }
        SocketAddr::V6(addr) => {
            let ip = addr.ip().segments();
            IpSocketAddress::Ipv6(Ipv6SocketAddress {
                address: (ip[0], ip[1], ip[2], ip[3], ip[4], ip[5], ip[6], ip[7]),
                port: addr.port(),
                flow_info: addr.flowinfo(),
                scope_id: addr.scope_id(),
            })
        }
    }
}

fn sockaddr_from_wasi(addr: IpSocketAddress) -> SocketAddr {
    match addr {
        IpSocketAddress::Ipv4(Ipv4SocketAddress { address, port }) => {
            SocketAddr::from(([address.0, address.1, address.2, address.3], port))
        }
        IpSocketAddress::Ipv6(Ipv6SocketAddress {
            address,
            port,
            flow_info,
            scope_id,
        }) => SocketAddr::V6(std::net::SocketAddrV6::new(
            std::net::Ipv6Addr::new(
                address.0, address.1, address.2, address.3, address.4, address.5, address.6,
                address.7,
            ),
            port,
            flow_info,
            scope_id,
        )),
    }
}

fn family(addr: &SocketAddr) -> IpAddressFamily {
    match addr {
        SocketAddr::V4(_) => IpAddressFamily::Ipv4,
        SocketAddr::V6(_) => IpAddressFamily::Ipv6,
    }
}

fn first_address(addr: impl ToSocketAddrs) -> io::Result<SocketAddr> {
    addr.to_socket_addrs()?
        .next()
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "no address"))
}

pub struct TcpStream {
    input_ready: Subscription,
    output_ready: Subscription,
    input: InputStream,
    output: OutputStream,
    socket: TcpSocket,
}

impl TcpStream {
    pub async fn connect(addr: impl ToSocketAddrs) -> io::Result<Self> {
        let addrs: Vec<SocketAddr> = addr.to_socket_addrs()?.collect();
        let mut last = None;
        for addr in addrs {
            match Self::connect_one(addr).await {
                Ok(stream) => return Ok(stream),
                Err(err) => last = Some(err),
            }
        }
        Err(last.unwrap_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "no address")))
    }

    async fn connect_one(addr: SocketAddr) -> io::Result<Self> {
        let socket = create_tcp_socket(family(&addr)).map_err(to_io_err)?;
        let network = instance_network();
        socket
            .start_connect(&network, sockaddr_to_wasi(addr))
            .map_err(to_io_err)?;
        wait(socket.subscribe()).await;
        let (input, output) = socket.finish_connect().map_err(to_io_err)?;
        Ok(Self {
            input_ready: Subscription::new(),
            output_ready: Subscription::new(),
            input,
            output,
            socket,
        })
    }

    pub fn set_nodelay(&self, _nodelay: bool) -> io::Result<()> {
        Ok(())
    }

    pub fn peer_addr(&self) -> io::Result<SocketAddr> {
        self.socket
            .remote_address()
            .map(sockaddr_from_wasi)
            .map_err(to_io_err)
    }
}

impl Drop for TcpStream {
    fn drop(&mut self) {
        let _ = self.socket.shutdown(ShutdownType::Both);
    }
}

impl AsyncRead for TcpStream {
    fn poll_read(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        let this = self.get_mut();
        if buf.remaining() == 0 {
            return Poll::Ready(Ok(()));
        }
        loop {
            if this
                .input_ready
                .poll(cx, || this.input.subscribe())
                .is_pending()
            {
                return Poll::Pending;
            }
            match this.input.read(buf.remaining() as u64) {
                Ok(data) if data.is_empty() => continue,
                Ok(data) => {
                    buf.put_slice(&data);
                    return Poll::Ready(Ok(()));
                }
                Err(StreamError::Closed) => return Poll::Ready(Ok(())),
                Err(err) => return Poll::Ready(Err(stream_err(err))),
            }
        }
    }
}

impl AsyncWrite for TcpStream {
    fn poll_write(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<io::Result<usize>> {
        let this = self.get_mut();
        loop {
            let ready = this.output_ready.poll(cx, || this.output.subscribe());
            match this.output.check_write() {
                Ok(0) => {
                    if ready.is_pending() {
                        return Poll::Pending;
                    }
                }
                Ok(writable) => {
                    let len = (writable as usize).min(buf.len());
                    match this.output.write(&buf[..len]) {
                        Ok(()) => return Poll::Ready(Ok(len)),
                        Err(err) => return Poll::Ready(Err(stream_err(err))),
                    }
                }
                Err(err) => return Poll::Ready(Err(stream_err(err))),
            }
        }
    }

    fn poll_flush(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        let this = self.get_mut();
        if let Err(err) = this.output.flush() {
            return Poll::Ready(Err(stream_err(err)));
        }
        this.output_ready
            .poll(cx, || this.output.subscribe())
            .map(|()| Ok(()))
    }

    fn poll_shutdown(self: Pin<&mut Self>, _cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        let _ = self.socket.shutdown(ShutdownType::Send);
        Poll::Ready(Ok(()))
    }
}

pub struct TcpListener {
    ready: Mutex<Option<usize>>,
    socket: TcpSocket,
}

impl TcpListener {
    pub async fn bind(addr: impl ToSocketAddrs) -> io::Result<Self> {
        let addr = first_address(addr)?;
        let socket = create_tcp_socket(family(&addr)).map_err(to_io_err)?;
        let network = instance_network();
        socket
            .start_bind(&network, sockaddr_to_wasi(addr))
            .map_err(to_io_err)?;
        wait(socket.subscribe()).await;
        socket.finish_bind().map_err(to_io_err)?;
        socket.start_listen().map_err(to_io_err)?;
        wait(socket.subscribe()).await;
        socket.finish_listen().map_err(to_io_err)?;
        Ok(Self {
            ready: Mutex::new(None),
            socket,
        })
    }

    pub fn local_addr(&self) -> io::Result<SocketAddr> {
        self.socket
            .local_address()
            .map(sockaddr_from_wasi)
            .map_err(to_io_err)
    }

    pub async fn accept(&self) -> io::Result<(TcpStream, SocketAddr)> {
        core::future::poll_fn(|cx| self.poll_accept(cx)).await
    }

    fn poll_accept(&self, cx: &mut Context<'_>) -> Poll<io::Result<(TcpStream, SocketAddr)>> {
        loop {
            let key = {
                let mut slot = self.ready.lock().unwrap();
                *slot.get_or_insert_with(|| reactor().insert(self.socket.subscribe()))
            };
            if !reactor().ready(key, cx.waker()) {
                return Poll::Pending;
            }
            match self.socket.accept() {
                Ok((socket, input, output)) => {
                    let peer = socket
                        .remote_address()
                        .map(sockaddr_from_wasi)
                        .unwrap_or_else(|_| SocketAddr::from(([0, 0, 0, 0], 0)));
                    let stream = TcpStream {
                        input_ready: Subscription::new(),
                        output_ready: Subscription::new(),
                        input,
                        output,
                        socket,
                    };
                    return Poll::Ready(Ok((stream, peer)));
                }
                Err(ErrorCode::WouldBlock) => continue,
                Err(err) => return Poll::Ready(Err(to_io_err(err))),
            }
        }
    }
}

impl Drop for TcpListener {
    fn drop(&mut self) {
        if let Some(key) = self.ready.lock().unwrap().take() {
            reactor().remove(key);
        }
    }
}
