use std::sync::Arc;
use std::time::{Duration, Instant};

use tokio::sync::Mutex;

use crate::openvpn::config::{Auth, Cipher, Profile, Protocol, Server};
use crate::openvpn::crypto::{DataCipher, KeyMaterial, PeerId, ReplayWindow, SeedData};
use crate::openvpn::error::Error;
use crate::openvpn::framing;
use crate::openvpn::tls::{CredentialStore, TlsState};

pub const P_CONTROL_HARD_RESET_CLIENT_V2: u8 = 7;
pub const P_CONTROL_HARD_RESET_SERVER_V2: u8 = 8;
pub const P_CONTROL_V1: u8 = 4;
pub const P_ACK_V1: u8 = 5;
pub const P_DATA_V1: u8 = 6;
pub const P_DATA_V2: u8 = 9;

pub const SID_SIZE: usize = 8;

const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(20);
const MAX_ACKS: usize = 4;
const MAX_CONTROL_PAYLOAD: usize = 1184;
const KEY_SOURCE_CLIENT_LEN: usize = 48 + 32 + 32;
const KEY_SOURCE_SERVER_LEN: usize = 32 + 32;

pub struct Control {
    transport: Arc<Mutex<Transport>>,
    tls: TlsState,
    session_id: [u8; SID_SIZE],
    remote_session_id: [u8; SID_SIZE],
    next_packet_id: u32,
    cipher: Cipher,
    auth: Auth,
    keys: Option<Keys>,
    seed: Option<SeedData>,
    peer_id: PeerId,
    send_counter: u32,
    replay: ReplayWindow,
    acked: Vec<u32>,
    last_payload: Vec<u8>,
    recv_next: u32,
    reorder: std::collections::BTreeMap<u32, Vec<u8>>,
}

struct Keys {
    encrypt: DataCipher,
    decrypt: DataCipher,
}

enum Transport {
    Udp(tokio::net::UdpSocket),
    Tcp(tokio::net::TcpStream),
}

impl Control {
    pub async fn connect(profile: &Profile, credentials: &CredentialStore) -> Result<Self, Error> {
        let mut last = None;
        for server in &profile.servers {
            match Self::connect_to(profile, credentials, server).await {
                Ok(control) => return Ok(control),
                Err(err) => {
                    tracing::debug!(
                        "openvpn {}:{} ({}) failed: {err}",
                        server.host,
                        server.port,
                        server.protocol.as_str()
                    );
                    last = Some(err);
                }
            }
        }
        Err(last.unwrap_or_else(|| Error::Account("no remote".into())))
    }

    async fn connect_to(
        profile: &Profile,
        credentials: &CredentialStore,
        server: &Server,
    ) -> Result<Self, Error> {
        let mut session_id = [0u8; SID_SIZE];
        rand::fill(&mut session_id);

        let transport = match server.protocol {
            Protocol::Udp => {
                let socket = tokio::net::UdpSocket::bind(("0.0.0.0", 0)).await?;
                socket.connect((server.host.as_str(), server.port)).await?;
                Transport::Udp(socket)
            }
            Protocol::Tcp => {
                let stream =
                    tokio::net::TcpStream::connect((server.host.as_str(), server.port)).await?;
                stream.set_nodelay(true)?;
                Transport::Tcp(stream)
            }
        };
        let transport = Arc::new(Mutex::new(transport));

        send_raw(&transport, &reset_packet(session_id)).await?;
        let (remote_session_id, reset_id) = await_reset(&transport, HANDSHAKE_TIMEOUT).await?;
        tracing::debug!(
            "openvpn {}:{} ({}): reset received",
            server.host,
            server.port,
            server.protocol.as_str()
        );

        let context = credentials.context(&server.host)?;
        let mut control = Self {
            transport,
            tls: TlsState::start(&context, &server.host)?,
            session_id,
            remote_session_id,
            next_packet_id: 1,
            cipher: profile.cipher,
            auth: profile.auth,
            keys: None,
            seed: None,
            peer_id: PeerId::NONE,
            send_counter: 0,
            replay: ReplayWindow::new(),
            acked: vec![reset_id],
            last_payload: Vec::new(),
            recv_next: reset_id.wrapping_add(1),
            reorder: std::collections::BTreeMap::new(),
        };
        control.drive_tls(HANDSHAKE_TIMEOUT).await?;
        tracing::debug!("openvpn: tls handshake done");
        control.exchange_keys(profile).await?;
        tracing::debug!("openvpn: key exchange done");
        Ok(control)
    }

    pub fn cipher(&self) -> Cipher {
        self.cipher
    }

    pub async fn push_request(&mut self) -> Result<String, Error> {
        self.tls.write(b"PUSH_REQUEST\0")?;
        self.flush_tls().await?;
        let deadline = Instant::now() + HANDSHAKE_TIMEOUT;
        let mut buffer = Vec::new();
        loop {
            let remaining = deadline.saturating_duration_since(Instant::now());
            if remaining.is_zero() {
                return Err(Error::Timeout);
            }
            self.pump(remaining).await?;
            if let Some(chunk) = self.tls.take_plaintext() {
                buffer.extend_from_slice(&chunk);
            }
            if let Some(reply) = take_message(&mut buffer, b"PUSH_REPLY") {
                return Ok(reply);
            }
            if let Some(error) = take_message(&mut buffer, b"AUTH_FAILED") {
                tracing::warn!("openvpn auth failed: {error}");
                return Err(Error::Auth);
            }
        }
    }

    pub fn apply_push(&mut self, reply: &str) {
        let pushed = parse_push(reply);
        if let Some(peer_id) = pushed.peer_id {
            self.peer_id = peer_id;
        }
        if let Some(cipher) = pushed.cipher {
            self.cipher = cipher;
        }
    }

    pub async fn install_keys(&mut self) -> Result<(), Error> {
        let seed = self
            .seed
            .take()
            .ok_or_else(|| Error::Control("keys already installed".into()))?;
        let keys = KeyMaterial::derive(&seed.as_seed(), self.cipher, self.auth.size());
        self.keys = Some(Keys {
            encrypt: DataCipher::new(self.cipher, self.auth, &keys.encrypt, &keys.mac_encrypt)?,
            decrypt: DataCipher::new(self.cipher, self.auth, &keys.decrypt, &keys.mac_decrypt)?,
        });
        Ok(())
    }

    async fn exchange_keys(&mut self, profile: &Profile) -> Result<(), Error> {
        let mut pre_master = [0u8; 48];
        rand::fill(&mut pre_master);
        let mut random1 = [0u8; 32];
        rand::fill(&mut random1);
        let mut random2 = [0u8; 32];
        rand::fill(&mut random2);

        let mut payload = Vec::with_capacity(128);
        payload.extend_from_slice(&0u32.to_be_bytes());
        payload.push(2);
        debug_assert_eq!(KEY_SOURCE_CLIENT_LEN, 112);
        payload.extend_from_slice(&pre_master);
        payload.extend_from_slice(&random1);
        payload.extend_from_slice(&random2);
        let options = format!(
            "V4,dev-type tun,link-mtu 1557,tun-mtu 1500,proto UDPv4,auth {},keysize {},key-method 2,tls-client",
            profile.auth.as_str(),
            profile.cipher.key_len() * 8
        );
        let peer_info = "IV_VER=2.6.9\nIV_PLAT=linux\nIV_TCPNL=1\nIV_NCP=2\n\
             IV_CIPHERS=AES-256-GCM:AES-128-GCM:AES-256-CBC:AES-128-CBC\nIV_PROTO=2180\nIV_MTU=1500\n\
             IV_SSL=OpenSSL\nIV_HWADDR=00:00:00:00:00:00\nIV_ACC=1,A:6,udp\nIV_IPV6=1\n"
            .to_string();
        write_string(&mut payload, &options)?;
        write_string(&mut payload, "")?;
        write_string(&mut payload, "")?;
        write_string(&mut payload, &peer_info)?;

        self.tls.write(&payload)?;
        let pending = self.tls.take_outbound();
        self.send_control_chunks(&pending).await?;

        let deadline = Instant::now() + HANDSHAKE_TIMEOUT;
        let mut buffer = Vec::new();
        let (server_random1, server_random2) = loop {
            let remaining = deadline.saturating_duration_since(Instant::now());
            if remaining.is_zero() {
                return Err(Error::Timeout);
            }
            self.pump(remaining).await?;
            if let Some(chunk) = self.tls.take_plaintext() {
                buffer.extend_from_slice(&chunk);
            }
            if buffer.is_empty() {
                continue;
            }
            if buffer.len() < 5 + KEY_SOURCE_SERVER_LEN {
                continue;
            }
            let mut reader = Reader::new(&buffer);
            let _ = reader.u32();
            let method = reader.u8();
            if method != 2 {
                return Err(Error::Control(format!("key method {method}")));
            }
            let Some(rest) = reader.take(KEY_SOURCE_SERVER_LEN) else {
                continue;
            };
            let mut random1 = [0u8; 32];
            let mut random2 = [0u8; 32];
            random1.copy_from_slice(&rest[0..32]);
            random2.copy_from_slice(&rest[32..64]);
            break (random1, random2);
        };

        let mut client_sid = [0u8; SID_SIZE];
        client_sid.copy_from_slice(&self.session_id);
        let mut server_sid = [0u8; SID_SIZE];
        server_sid.copy_from_slice(&self.remote_session_id);

        self.seed = Some(SeedData {
            pre_master,
            client_random1: random1,
            server_random1,
            client_random2: random2,
            server_random2,
            client_sid,
            server_sid,
        });
        Ok(())
    }

    async fn drive_tls(&mut self, timeout: Duration) -> Result<(), Error> {
        let deadline = Instant::now() + timeout;
        loop {
            self.tls.drive()?;
            let outbound = self.tls.take_outbound();
            if !outbound.is_empty() {
                self.send_control_chunks(&outbound).await?;
            }
            if self.tls.is_ready() {
                return Ok(());
            }
            let remaining = deadline.saturating_duration_since(Instant::now());
            if remaining.is_zero() {
                return Err(Error::Timeout);
            }
            self.pump(remaining).await?;
        }
    }

    async fn flush_tls(&mut self) -> Result<(), Error> {
        let outbound = self.tls.take_outbound();
        if outbound.is_empty() {
            return Ok(());
        }
        self.send_control_chunks(&outbound).await
    }

    async fn retransmit_control(&mut self) -> Result<(), Error> {
        let payload = self.last_payload.clone();
        if payload.is_empty() {
            return Ok(());
        }
        let packet_id = self.next_packet_id;
        self.next_packet_id = self.next_packet_id.wrapping_add(1);
        let mut out = Vec::with_capacity(1 + SID_SIZE + 1 + 4 + payload.len());
        out.push(P_CONTROL_V1 << 3);
        out.extend_from_slice(&self.session_id);
        out.push(0);
        out.extend_from_slice(&packet_id.to_be_bytes());
        out.extend_from_slice(&payload);
        self.send_raw(&out).await
    }

    async fn send_control_chunks(&mut self, data: &[u8]) -> Result<(), Error> {
        let mut offset = 0;
        while offset < data.len() {
            let end = (offset + MAX_CONTROL_PAYLOAD).min(data.len());
            self.send_control(&data[offset..end]).await?;
            offset = end;
        }
        Ok(())
    }

    async fn pump(&mut self, timeout: Duration) -> Result<(), Error> {
        let deadline = Instant::now() + timeout;
        let mut last_resend = Instant::now();
        loop {
            let remaining = deadline.saturating_duration_since(Instant::now());
            if remaining.is_zero() {
                return Err(Error::Timeout);
            }
            let wait = remaining.min(Duration::from_millis(500));
            let Some(packet) = self.recv_packet(wait).await? else {
                if !self.tls.is_ready() && last_resend.elapsed() > Duration::from_secs(2) {
                    last_resend = Instant::now();
                    self.retransmit_control().await?;
                }
                continue;
            };
            match packet.opcode {
                P_CONTROL_V1 => {
                    self.send_ack(packet.packet_id).await?;
                    self.accept_control(packet.packet_id, packet.payload)?;
                    self.tls.drive()?;
                    self.flush_tls().await?;
                    return Ok(());
                }
                P_ACK_V1 => {}
                P_CONTROL_HARD_RESET_SERVER_V2 => {}
                _ => {}
            }
        }
    }

    pub async fn send_data(&mut self, payload: &[u8]) -> Result<(), Error> {
        self.send_counter = self.send_counter.wrapping_add(1);
        let counter = self.send_counter;
        let mut header = data_header(self.cipher, 0, self.peer_id);
        let keys = self
            .keys
            .as_ref()
            .ok_or_else(|| Error::Control("data keys missing".into()))?;
        header.extend_from_slice(&counter.to_be_bytes());
        let encrypted = keys.encrypt.encrypt(&header, counter, payload)?;
        let mut out = Vec::with_capacity(header.len() + encrypted.len());
        out.extend_from_slice(&header);
        out.extend_from_slice(&encrypted);
        self.send_raw(&out).await
    }

    pub async fn recv_data(&mut self, timeout: Duration) -> Result<Option<Vec<u8>>, Error> {
        let deadline = Instant::now() + timeout;
        loop {
            let remaining = deadline.saturating_duration_since(Instant::now());
            if remaining.is_zero() {
                return Ok(None);
            }
            let Some(packet) = self.recv_packet(remaining).await? else {
                return Ok(None);
            };
            match packet.opcode {
                P_DATA_V1 | P_DATA_V2 => {
                    let Some(keys) = self.keys.as_ref() else {
                        continue;
                    };
                    let (counter, plaintext) =
                        keys.decrypt.decrypt(&packet.header, &packet.payload)?;
                    if !self.replay.accept(counter) {
                        continue;
                    }
                    return Ok(Some(plaintext));
                }
                P_CONTROL_V1 => {
                    self.send_ack(packet.packet_id).await?;
                    self.accept_control(packet.packet_id, packet.payload)?;
                    self.tls.drive()?;
                    self.flush_tls().await?;
                }
                P_CONTROL_HARD_RESET_SERVER_V2 => {
                    self.send_ack(packet.packet_id).await?;
                }
                _ => {}
            }
        }
    }

    fn accept_control(&mut self, packet_id: u32, payload: Vec<u8>) -> Result<(), Error> {
        if packet_id < self.recv_next {
            return Ok(());
        }
        if packet_id > self.recv_next {
            if self.reorder.len() < 32 {
                self.reorder.insert(packet_id, payload);
            }
            return Ok(());
        }
        self.tls.feed(&payload)?;
        self.recv_next = self.recv_next.wrapping_add(1);
        while let Some(payload) = self.reorder.remove(&self.recv_next) {
            self.tls.feed(&payload)?;
            self.recv_next = self.recv_next.wrapping_add(1);
        }
        Ok(())
    }

    async fn send_control(&mut self, payload: &[u8]) -> Result<(), Error> {
        let packet_id = self.next_packet_id;
        self.next_packet_id = self.next_packet_id.wrapping_add(1);
        let mut out = Vec::with_capacity(1 + SID_SIZE + 1 + 4 + SID_SIZE + 4 + payload.len());
        out.push(P_CONTROL_V1 << 3);
        out.extend_from_slice(&self.session_id);
        self.write_acks(&mut out);
        out.extend_from_slice(&packet_id.to_be_bytes());
        out.extend_from_slice(payload);
        self.last_payload = payload.to_vec();
        self.send_raw(&out).await
    }

    fn record_ack(&mut self, packet_id: u32) {
        if self.acked.contains(&packet_id) {
            return;
        }
        self.acked.insert(0, packet_id);
        self.acked.truncate(MAX_ACKS);
    }

    async fn send_ack(&mut self, packet_id: u32) -> Result<(), Error> {
        self.record_ack(packet_id);
        let mut out = Vec::with_capacity(1 + SID_SIZE + 1 + 4 * MAX_ACKS + SID_SIZE);
        out.push(P_ACK_V1 << 3);
        out.extend_from_slice(&self.session_id);
        let acks = encode_acks(&self.acked, &self.remote_session_id);
        out.extend_from_slice(&acks);
        self.send_raw(&out).await
    }

    fn write_acks(&self, out: &mut Vec<u8>) {
        let acks = encode_acks(&self.acked, &self.remote_session_id);
        out.extend_from_slice(&acks);
    }

    async fn send_raw(&self, payload: &[u8]) -> Result<(), Error> {
        let mut guard = self.transport.lock().await;
        match &mut *guard {
            Transport::Udp(socket) => {
                socket.send(payload).await?;
            }
            Transport::Tcp(stream) => framing::write_packet(stream, payload).await?,
        }
        Ok(())
    }

    async fn recv_packet(&mut self, timeout: Duration) -> Result<Option<Packet>, Error> {
        let mut guard = self.transport.lock().await;
        let raw = match &mut *guard {
            Transport::Udp(socket) => {
                let mut buf = vec![0u8; 2048];
                match tokio::time::timeout(timeout, socket.recv(&mut buf)).await {
                    Ok(Ok(size)) => {
                        buf.truncate(size);
                        buf
                    }
                    Ok(Err(err)) => return Err(Error::Io(err)),
                    Err(_) => return Ok(None),
                }
            }
            Transport::Tcp(stream) => {
                match tokio::time::timeout(timeout, framing::read_packet(stream)).await {
                    Ok(Ok(packet)) => packet,
                    Ok(Err(err)) => return Err(err),
                    Err(_) => return Ok(None),
                }
            }
        };
        Ok(Packet::parse(&raw))
    }
}

async fn send_raw(transport: &Arc<Mutex<Transport>>, payload: &[u8]) -> Result<(), Error> {
    let mut guard = transport.lock().await;
    match &mut *guard {
        Transport::Udp(socket) => {
            socket.send(payload).await?;
        }
        Transport::Tcp(stream) => framing::write_packet(stream, payload).await?,
    }
    Ok(())
}

async fn await_reset(
    transport: &Arc<Mutex<Transport>>,
    timeout: Duration,
) -> Result<([u8; SID_SIZE], u32), Error> {
    let deadline = Instant::now() + timeout;
    loop {
        let remaining = deadline.saturating_duration_since(Instant::now());
        if remaining.is_zero() {
            return Err(Error::Timeout);
        }
        let raw = {
            let mut guard = transport.lock().await;
            match &mut *guard {
                Transport::Udp(socket) => {
                    let mut buf = vec![0u8; 2048];
                    match tokio::time::timeout(remaining, socket.recv(&mut buf)).await {
                        Ok(Ok(size)) => {
                            buf.truncate(size);
                            buf
                        }
                        Ok(Err(err)) => return Err(Error::Io(err)),
                        Err(_) => continue,
                    }
                }
                Transport::Tcp(stream) => {
                    match tokio::time::timeout(remaining, framing::read_packet(stream)).await {
                        Ok(Ok(packet)) => packet,
                        Ok(Err(err)) => return Err(err),
                        Err(_) => continue,
                    }
                }
            }
        };
        let Some(packet) = Packet::parse(&raw) else {
            continue;
        };
        if packet.opcode == P_CONTROL_HARD_RESET_SERVER_V2 {
            let mut sid = [0u8; SID_SIZE];
            sid.copy_from_slice(&packet.session_id);
            return Ok((sid, packet.packet_id));
        }
    }
}

pub fn reset_packet(session_id: [u8; SID_SIZE]) -> Vec<u8> {
    let mut out = Vec::with_capacity(1 + SID_SIZE + 1 + 4);
    out.push(P_CONTROL_HARD_RESET_CLIENT_V2 << 3);
    out.extend_from_slice(&session_id);
    out.push(0);
    out.extend_from_slice(&0u32.to_be_bytes());
    out
}

pub struct Packet {
    pub opcode: u8,
    pub session_id: Vec<u8>,
    pub packet_id: u32,
    pub header: Vec<u8>,
    pub payload: Vec<u8>,
}

impl Packet {
    pub fn parse(raw: &[u8]) -> Option<Self> {
        if raw.is_empty() {
            return None;
        }
        let byte = raw[0];
        let opcode = byte >> 3;
        if opcode == P_DATA_V1 || opcode == P_DATA_V2 {
            let header_len = if opcode == P_DATA_V2 { 4 } else { 1 };
            if raw.len() < header_len + 4 {
                return None;
            }
            return Some(Self {
                opcode,
                session_id: Vec::new(),
                packet_id: 0,
                header: raw[..header_len + 4].to_vec(),
                payload: raw[header_len + 4..].to_vec(),
            });
        }
        if raw.len() < 1 + SID_SIZE + 1 {
            return None;
        }
        let session_id = raw[1..1 + SID_SIZE].to_vec();
        let mut offset = 1 + SID_SIZE;
        let count = raw[offset] as usize;
        offset += 1;
        if raw.len() < offset + count * 4 {
            return None;
        }
        offset += count * 4;
        if count > 0 {
            offset += SID_SIZE;
        }
        let packet_id = if opcode == P_ACK_V1 {
            0
        } else {
            if raw.len() < offset + 4 {
                return None;
            }
            let id = u32::from_be_bytes(raw[offset..offset + 4].try_into().ok()?);
            offset += 4;
            id
        };
        Some(Self {
            opcode,
            session_id,
            packet_id,
            header: Vec::new(),
            payload: raw[offset..].to_vec(),
        })
    }
}

pub fn write_string(out: &mut Vec<u8>, value: &str) -> Result<(), Error> {
    if value.len() > u16::MAX as usize {
        return Err(Error::Control("string too long".into()));
    }
    out.extend_from_slice(&(value.len() as u16).to_be_bytes());
    out.extend_from_slice(value.as_bytes());
    Ok(())
}

pub fn data_header(cipher: Cipher, key_id: u8, peer_id: PeerId) -> Vec<u8> {
    if cipher.is_aead() {
        let mut out = vec![(P_DATA_V2 << 3) | (key_id & 0x07)];
        let id = peer_id.0.to_be_bytes();
        out.extend_from_slice(&id[1..4]);
        out
    } else {
        vec![(P_DATA_V1 << 3) | (key_id & 0x07)]
    }
}

fn encode_acks(packet_ids: &[u32], remote_session_id: &[u8; SID_SIZE]) -> Vec<u8> {
    let count = packet_ids.len().min(MAX_ACKS);
    let mut out = Vec::with_capacity(1 + count * 4 + SID_SIZE);
    out.push(count as u8);
    for packet_id in &packet_ids[..count] {
        out.extend_from_slice(&packet_id.to_be_bytes());
    }
    if count > 0 {
        out.extend_from_slice(remote_session_id);
    }
    out
}

pub fn parse_push(reply: &str) -> Pushed {
    let mut pushed = Pushed::default();
    for line in reply.split(',') {
        let line = line.trim();
        if let Some(peer) = line.strip_prefix("peer-id ") {
            if let Ok(id) = peer.trim().parse::<u32>() {
                pushed.peer_id = Some(PeerId(id));
            }
        } else if let Some(name) = line.strip_prefix("cipher ") {
            if let Some(cipher) = Cipher::parse(name) {
                pushed.cipher = Some(cipher);
            }
        } else if let Some(names) = line.strip_prefix("data-ciphers ") {
            for name in names.split(':') {
                if let Some(parsed) = Cipher::parse(name) {
                    pushed.cipher = Some(parsed);
                    break;
                }
            }
        } else if let Some(rest) = line.strip_prefix("ifconfig ") {
            let mut parts = rest.split_whitespace();
            pushed.local = parts.next().and_then(|value| value.parse().ok());
            pushed.remote = parts.next().and_then(|value| value.parse().ok());
        }
    }
    pushed
}

#[derive(Debug, Default, PartialEq, Eq)]
pub struct Pushed {
    pub cipher: Option<Cipher>,
    pub peer_id: Option<PeerId>,
    pub local: Option<std::net::Ipv4Addr>,
    pub remote: Option<std::net::Ipv4Addr>,
}

struct Reader<'a> {
    data: &'a [u8],
    offset: usize,
}

impl<'a> Reader<'a> {
    fn new(data: &'a [u8]) -> Self {
        Self { data, offset: 0 }
    }

    fn u8(&mut self) -> u8 {
        let value = self.data.get(self.offset).copied().unwrap_or(0);
        self.offset += 1;
        value
    }

    fn u32(&mut self) -> u32 {
        if self.data.len() < self.offset + 4 {
            return 0;
        }
        let value = u32::from_be_bytes(
            self.data[self.offset..self.offset + 4]
                .try_into()
                .expect("u32"),
        );
        self.offset += 4;
        value
    }

    fn take(&mut self, len: usize) -> Option<&'a [u8]> {
        if self.data.len() < self.offset + len {
            return None;
        }
        let slice = &self.data[self.offset..self.offset + len];
        self.offset += len;
        Some(slice)
    }
}

fn take_message(buffer: &mut Vec<u8>, prefix: &[u8]) -> Option<String> {
    let start = buffer
        .windows(prefix.len())
        .position(|window| window == prefix)?;
    let end = buffer[start..]
        .iter()
        .position(|byte| *byte == 0)
        .map(|offset| start + offset)?;
    let message = String::from_utf8_lossy(&buffer[start..end]).into_owned();
    buffer.drain(..=end);
    Some(message)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn hex(value: &str) -> Vec<u8> {
        (0..value.len())
            .step_by(2)
            .map(|index| u8::from_str_radix(&value[index..index + 2], 16).expect("hex"))
            .collect()
    }

    #[test]
    fn reset_packet_has_empty_ack_block() {
        let packet = reset_packet([7u8; SID_SIZE]);
        assert_eq!(packet[0], P_CONTROL_HARD_RESET_CLIENT_V2 << 3);
        assert_eq!(&packet[1..9], &[7u8; SID_SIZE]);
        assert_eq!(packet[9], 0, "ack count");
        assert_eq!(&packet[10..], &[0u8, 0, 0, 0], "packet id");
    }

    #[test]
    fn encodes_ack_record() {
        assert_eq!(encode_acks(&[], &[9u8; SID_SIZE]), vec![0u8]);
        let acked = encode_acks(&[1, 2], &[9u8; SID_SIZE]);
        assert_eq!(acked[0], 2);
        assert_eq!(&acked[1..5], &1u32.to_be_bytes());
        assert_eq!(&acked[5..9], &2u32.to_be_bytes());
        assert_eq!(&acked[9..], &[9u8; SID_SIZE]);
    }

    #[test]
    fn parses_control_packet() {
        let mut raw = vec![(P_CONTROL_V1 << 3) | 1];
        raw.extend_from_slice(&[9u8; SID_SIZE]);
        raw.push(1);
        raw.extend_from_slice(&3u32.to_be_bytes());
        raw.extend_from_slice(&[4u8; SID_SIZE]);
        raw.extend_from_slice(&7u32.to_be_bytes());
        raw.extend_from_slice(b"payload");
        let packet = Packet::parse(&raw).expect("packet");
        assert_eq!(packet.opcode, P_CONTROL_V1);
        assert_eq!(packet.session_id, vec![9u8; SID_SIZE]);
        assert_eq!(packet.packet_id, 7);
        assert_eq!(packet.payload, b"payload");
    }

    #[test]
    fn parses_ack_packet() {
        let mut raw = vec![P_ACK_V1 << 3];
        raw.extend_from_slice(&[9u8; SID_SIZE]);
        raw.push(1);
        raw.extend_from_slice(&5u32.to_be_bytes());
        raw.extend_from_slice(&[4u8; SID_SIZE]);
        let packet = Packet::parse(&raw).expect("packet");
        assert_eq!(packet.opcode, P_ACK_V1);
        assert_eq!(packet.packet_id, 0);
        assert!(packet.payload.is_empty());
    }

    #[test]
    fn parses_data_packet() {
        let raw = [0x48u8, 0, 0, 5, 0, 0, 0, 1, 1, 2, 3, 4];
        let packet = Packet::parse(&raw).expect("packet");
        assert_eq!(packet.opcode, P_DATA_V2);
        assert_eq!(packet.header, vec![0x48, 0, 0, 5, 0, 0, 0, 1]);
        assert_eq!(packet.payload, vec![1, 2, 3, 4]);
    }

    #[test]
    fn data_header_follows_cipher_mode() {
        assert_eq!(
            data_header(Cipher::Aes128Cbc, 0, PeerId(0x0a0b0c)),
            vec![P_DATA_V1 << 3]
        );
        assert_eq!(
            data_header(Cipher::Aes256Gcm, 1, PeerId(0x0a0b0c)),
            vec![(P_DATA_V2 << 3) | 1, 0x0a, 0x0b, 0x0c]
        );
    }

    #[test]
    fn parses_push_reply() {
        let pushed = parse_push(
            "PUSH_REPLY,route-gateway 10.8.0.1,ifconfig 10.8.0.6 255.255.255.0,\
             peer-id 3,data-ciphers AES-256-CBC:AES-128-GCM,cipher AES-256-GCM",
        );
        assert_eq!(pushed.cipher, Some(Cipher::Aes256Gcm));
        assert_eq!(pushed.peer_id, Some(PeerId(3)));
        assert_eq!(pushed.local, Some(std::net::Ipv4Addr::new(10, 8, 0, 6)));
    }

    #[test]
    fn push_reply_without_options_keeps_defaults() {
        let pushed = parse_push("PUSH_REPLY,route 10.8.0.0 255.255.255.0");
        assert_eq!(pushed, Pushed::default());
    }

    #[test]
    fn writes_length_prefixed_string() {
        let mut out = Vec::new();
        write_string(&mut out, "abc").expect("string");
        assert_eq!(out, vec![0, 3, b'a', b'b', b'c']);
        assert_eq!(out.len(), 2 + 3);
    }

    #[test]
    fn finds_push_reply() {
        let mut buffer = b"PUSH_REPLY,route 1.2.3.4\0".to_vec();
        assert_eq!(
            take_message(&mut buffer, b"PUSH_REPLY").as_deref(),
            Some("PUSH_REPLY,route 1.2.3.4")
        );
        assert!(buffer.is_empty());
    }

    #[test]
    fn ignores_message_without_terminator() {
        let mut buffer = b"PUSH_REPLY,partial".to_vec();
        assert_eq!(take_message(&mut buffer, b"PUSH_REPLY"), None);
        assert_eq!(buffer, b"PUSH_REPLY,partial");
    }

    #[test]
    fn skips_bytes_before_message() {
        let mut buffer = hex("000102");
        buffer.extend_from_slice(b"AUTH_FAILED,bad\0tail");
        assert_eq!(
            take_message(&mut buffer, b"AUTH_FAILED").as_deref(),
            Some("AUTH_FAILED,bad")
        );
        assert_eq!(buffer, b"tail");
    }
}
