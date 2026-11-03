use std::collections::VecDeque;
use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Poll};
use std::time::Duration;

use smoltcp::iface::{Config, Interface, SocketHandle, SocketSet};
use smoltcp::phy::{Device, DeviceCapabilities, Medium, RxToken, TxToken};
use smoltcp::socket::tcp;
use smoltcp::time::Instant as SmolInstant;
use smoltcp::wire::{HardwareAddress, IpAddress, IpCidr, IpEndpoint, Ipv4Address};
use tokio::io::{AsyncRead, AsyncWrite, ReadBuf};
use tokio::sync::{mpsc, oneshot};

use crate::openvpn::config::Protocol;
use crate::openvpn::control::Control;
use crate::openvpn::error::Error;

const MTU: usize = 1500;
const QUEUE: usize = 16;
const CHUNK: usize = 16 * 1024;
const SOCKET_BUFFER: usize = 32 * 1024;
const TICK: Duration = Duration::from_millis(2);

struct DeviceState {
    inbound: VecDeque<Vec<u8>>,
    outbound: VecDeque<Vec<u8>>,
}

struct StackDevice<'a> {
    state: &'a mut DeviceState,
}

impl Device for StackDevice<'_> {
    type RxToken<'a>
        = StackRx
    where
        Self: 'a;
    type TxToken<'a>
        = StackTx<'a>
    where
        Self: 'a;

    fn receive(
        &mut self,
        _timestamp: SmolInstant,
    ) -> Option<(Self::RxToken<'_>, Self::TxToken<'_>)> {
        let packet = self.state.inbound.pop_front()?;
        Some((StackRx(packet), StackTx(self.state)))
    }

    fn transmit(&mut self, _timestamp: SmolInstant) -> Option<Self::TxToken<'_>> {
        Some(StackTx(self.state))
    }

    fn capabilities(&self) -> DeviceCapabilities {
        let mut caps = DeviceCapabilities::default();
        caps.medium = Medium::Ip;
        caps.max_transmission_unit = MTU;
        caps
    }
}

struct StackRx(Vec<u8>);

impl RxToken for StackRx {
    fn consume<R, F>(self, f: F) -> R
    where
        F: FnOnce(&[u8]) -> R,
    {
        f(&self.0)
    }
}

struct StackTx<'a>(&'a mut DeviceState);

impl TxToken for StackTx<'_> {
    fn consume<R, F>(self, len: usize, f: F) -> R
    where
        F: FnOnce(&mut [u8]) -> R,
    {
        let mut buffer = vec![0u8; len];
        let result = f(&mut buffer);
        self.0.outbound.push_back(buffer);
        result
    }
}

struct Clock(std::time::Instant);

impl Clock {
    fn now(&self) -> SmolInstant {
        SmolInstant::from_millis(self.0.elapsed().as_millis() as i64)
    }
}

enum Command {
    Open {
        target: IpEndpoint,
        reply: oneshot::Sender<Result<Stream, Error>>,
    },
}

struct Conn {
    handle: SocketHandle,
    to_client: mpsc::Sender<Vec<u8>>,
    from_client: mpsc::Receiver<Vec<u8>>,
    closed: bool,
}

pub struct Stack {
    commands: mpsc::Sender<Command>,
}

impl Stack {
    pub fn spawn(control: Control, local: Ipv4Address, protocol: Protocol) -> Arc<Self> {
        let (commands, rx) = mpsc::channel(QUEUE);
        let stack = Arc::new(Self { commands });
        tokio::spawn(run(control, local, protocol, rx));
        stack
    }

    pub async fn open(&self, host: &str, port: u16) -> Result<Stream, Error> {
        let address = resolve(host).await?;
        let target = IpEndpoint::new(address, port);
        let (reply, response) = oneshot::channel();
        self.commands
            .send(Command::Open { target, reply })
            .await
            .map_err(|_| Error::Transport("tunnel closed".into()))?;
        response
            .await
            .map_err(|_| Error::Transport("tunnel closed".into()))?
    }
}

async fn run(
    mut control: Control,
    local: Ipv4Address,
    protocol: Protocol,
    mut commands: mpsc::Receiver<Command>,
) {
    let clock = Clock(std::time::Instant::now());
    let mut device = DeviceState {
        inbound: VecDeque::with_capacity(QUEUE),
        outbound: VecDeque::with_capacity(QUEUE),
    };
    let mut config = Config::new(HardwareAddress::Ip);
    config.random_seed = rand::random();
    let mut iface = Interface::new(config, &mut StackDevice { state: &mut device }, clock.now());
    iface.update_ip_addrs(|addrs| {
        let _ = addrs.push(IpCidr::new(IpAddress::Ipv4(local), 24));
    });
    let mut sockets = SocketSet::new(Vec::new());
    let mut conns: Vec<Conn> = Vec::new();
    let mut ticker = tokio::time::interval(TICK);
    ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);

    loop {
        tokio::select! {
            biased;
            command = commands.recv() => {
                match command {
                    Some(Command::Open { target, reply }) => {
                        let socket = tcp::Socket::new(
                            tcp::SocketBuffer::new(vec![0u8; SOCKET_BUFFER]),
                            tcp::SocketBuffer::new(vec![0u8; SOCKET_BUFFER]),
                        );
                        let handle = sockets.add(socket);
                        let socket = sockets.get_mut::<tcp::Socket>(handle);
                        match socket.connect(iface.context(), target, rand::random::<u16>()) {
                            Ok(()) => {
                                let (to_client, read_rx) = mpsc::channel(QUEUE);
                                let (write_tx, from_client) = mpsc::channel(QUEUE);
                                conns.push(Conn {
                                    handle,
                                    to_client,
                                    from_client,
                                    closed: false,
                                });
                                let _ = reply.send(Ok(Stream::new(read_rx, write_tx)));
                            }
                            Err(err) => {
                                let _ = reply.send(Err(Error::Transport(format!("{err:?}"))));
                            }
                        }
                    }
                    None => break,
                }
                continue;
            }
            packet = control.recv_data(TICK) => {
                if let Ok(Some(packet)) = packet
                    && device.inbound.len() < QUEUE
                {
                    device.inbound.push_back(packet);
                }
            }
            _ = ticker.tick() => {}
        }

        iface.poll(
            clock.now(),
            &mut StackDevice { state: &mut device },
            &mut sockets,
        );

        while let Some(packet) = device.outbound.pop_front() {
            if let Err(err) = control.send_data(&packet).await {
                tracing::debug!("openvpn data send failed: {err}");
                break;
            }
        }

        for conn in conns.iter_mut() {
            let socket = sockets.get_mut::<tcp::Socket>(conn.handle);
            if socket.can_recv() && conn.to_client.capacity() > 0 {
                let mut buffer = vec![0u8; CHUNK];
                if let Ok(read) = socket.recv_slice(&mut buffer)
                    && read > 0
                {
                    buffer.truncate(read);
                    let _ = conn.to_client.try_send(buffer);
                }
            }
            if socket.can_send() {
                while let Ok(chunk) = conn.from_client.try_recv() {
                    match socket.send_slice(&chunk) {
                        Ok(written) if written == chunk.len() => {}
                        Ok(written) => {
                            let _ = conn.to_client.try_send(chunk[written..].to_vec());
                            break;
                        }
                        Err(_) => break,
                    }
                }
            }
            if !socket.is_open() {
                conn.closed = true;
            }
        }

        if conns.iter().any(|conn| conn.closed) {
            conns.retain(|conn| {
                if conn.closed {
                    sockets.remove(conn.handle);
                    false
                } else {
                    true
                }
            });
        }
    }

    let _ = protocol;
}

pub struct Stream {
    read: mpsc::Receiver<Vec<u8>>,
    write: mpsc::Sender<Vec<u8>>,
    pending: Vec<u8>,
    offset: usize,
}

impl Stream {
    fn new(read: mpsc::Receiver<Vec<u8>>, write: mpsc::Sender<Vec<u8>>) -> Self {
        Self {
            read,
            write,
            pending: Vec::new(),
            offset: 0,
        }
    }
}

impl AsyncRead for Stream {
    fn poll_read(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut ReadBuf<'_>,
    ) -> Poll<std::io::Result<()>> {
        let this = self.get_mut();
        if this.offset < this.pending.len() {
            let take = (this.pending.len() - this.offset).min(buf.remaining());
            buf.put_slice(&this.pending[this.offset..this.offset + take]);
            this.offset += take;
            if this.offset == this.pending.len() {
                this.pending.clear();
                this.offset = 0;
            }
            return Poll::Ready(Ok(()));
        }
        match this.read.poll_recv(cx) {
            Poll::Ready(Some(chunk)) => {
                let take = chunk.len().min(buf.remaining());
                buf.put_slice(&chunk[..take]);
                if take < chunk.len() {
                    this.pending = chunk;
                    this.offset = take;
                }
                Poll::Ready(Ok(()))
            }
            Poll::Ready(None) => Poll::Ready(Ok(())),
            Poll::Pending => Poll::Pending,
        }
    }
}

impl AsyncWrite for Stream {
    fn poll_write(
        self: Pin<&mut Self>,
        _cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<std::io::Result<usize>> {
        let this = self.get_mut();
        match this.write.try_send(buf.to_vec()) {
            Ok(()) => Poll::Ready(Ok(buf.len())),
            Err(mpsc::error::TrySendError::Full(_)) => {
                _cx.waker().wake_by_ref();
                Poll::Pending
            }
            Err(mpsc::error::TrySendError::Closed(_)) => {
                Poll::Ready(Err(std::io::Error::other("tunnel closed")))
            }
        }
    }

    fn poll_flush(self: Pin<&mut Self>, _cx: &mut Context<'_>) -> Poll<std::io::Result<()>> {
        Poll::Ready(Ok(()))
    }

    fn poll_shutdown(self: Pin<&mut Self>, _cx: &mut Context<'_>) -> Poll<std::io::Result<()>> {
        Poll::Ready(Ok(()))
    }
}

async fn resolve(host: &str) -> Result<IpAddress, Error> {
    if let Ok(address) = host.parse::<std::net::IpAddr>() {
        return Ok(to_smol(address));
    }
    let host = host.to_string();
    let resolved = tokio::task::spawn_blocking(move || {
        use std::net::ToSocketAddrs;
        (host.as_str(), 0u16)
            .to_socket_addrs()
            .ok()
            .and_then(|mut iter| iter.next())
            .map(|addr| addr.ip())
    })
    .await
    .map_err(|err| Error::Transport(err.to_string()))?;
    resolved
        .map(to_smol)
        .ok_or_else(|| Error::Transport("dns resolution failed".into()))
}

fn to_smol(address: std::net::IpAddr) -> IpAddress {
    match address {
        std::net::IpAddr::V4(v4) => IpAddress::Ipv4(v4),
        std::net::IpAddr::V6(v6) => IpAddress::Ipv6(v6),
    }
}
