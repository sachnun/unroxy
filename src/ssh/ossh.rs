use std::io;
use std::pin::Pin;
use std::task::{Context, Poll};

use bytes::{Buf, BytesMut};
use sha1::{Digest, Sha1};
use tokio::io::{AsyncRead, AsyncWrite, ReadBuf};

const SEED_LENGTH: usize = 16;
const KEY_LENGTH: usize = 16;
const HASH_ITERATIONS: usize = 6000;
const MAGIC: u32 = 0x0BF5CA7E;
const MAX_PACKET: usize = 256 * 1024;
const MAX_LINE: usize = 4096;
const MSG_NEWKEYS: u8 = 21;
const SHRINK_AT: usize = 256 * 1024;

struct Rc4 {
    s: [u8; 256],
    i: u8,
    j: u8,
}

impl Rc4 {
    fn new(key: &[u8]) -> Self {
        let mut s = [0u8; 256];
        for (i, v) in s.iter_mut().enumerate() {
            *v = i as u8;
        }
        let mut j = 0u8;
        for i in 0..256 {
            j = j.wrapping_add(s[i]).wrapping_add(key[i % key.len()]);
            s.swap(i, j as usize);
        }
        Self { s, i: 0, j: 0 }
    }

    fn apply(&mut self, buf: &mut [u8]) {
        for byte in buf.iter_mut() {
            self.i = self.i.wrapping_add(1);
            self.j = self.j.wrapping_add(self.s[self.i as usize]);
            self.s.swap(self.i as usize, self.j as usize);
            let k = self.s[self.s[self.i as usize].wrapping_add(self.s[self.j as usize]) as usize];
            *byte ^= k;
        }
    }
}

fn derive_key(seed: &[u8], keyword: &[u8], iv: &[u8]) -> [u8; KEY_LENGTH] {
    let mut hasher = Sha1::new();
    hasher.update(seed);
    hasher.update(keyword);
    hasher.update(iv);
    let mut digest = hasher.finalize();
    for _ in 0..HASH_ITERATIONS {
        let mut hasher = Sha1::new();
        hasher.update(digest);
        digest = hasher.finalize();
    }
    let mut key = [0u8; KEY_LENGTH];
    key.copy_from_slice(&digest[..KEY_LENGTH]);
    key
}

fn normalize_packet(packet: &[u8]) -> BytesMut {
    let packet_length = u32::from_be_bytes(packet[..4].try_into().unwrap()) as usize;
    let padding_length = packet[4] as usize;
    let payload_length = packet_length.saturating_sub(padding_length + 1);
    let payload_end = (5 + payload_length).min(packet.len());
    let payload = &packet[5..payload_end];
    let mut new_padding = 4usize;
    while !(payload.len() + 1 + new_padding + 4).is_multiple_of(8) {
        new_padding += 1;
    }
    let mut out = BytesMut::with_capacity(5 + payload.len() + new_padding);
    out.extend_from_slice(&((payload.len() + 1 + new_padding) as u32).to_be_bytes());
    out.extend_from_slice(&[new_padding as u8]);
    out.extend_from_slice(payload);
    out.resize(5 + payload.len() + new_padding, 0);
    out
}

fn find_crlf(buf: &[u8]) -> Option<usize> {
    buf.windows(2).position(|w| w == b"\r\n")
}

enum WriteState {
    Ident,
    Kex,
    Done,
}

enum ReadState {
    Ident,
    Kex,
    Done,
}

pub struct OsshStream<S> {
    inner: S,
    c2s: Rc4,
    s2c: Rc4,
    wstate: WriteState,
    wbuf: BytesMut,
    wout: BytesMut,
    rstate: ReadState,
    rin: BytesMut,
    rbuf: BytesMut,
    rline: Vec<u8>,
    kex_prefix: Option<(BytesMut, usize)>,
}

impl<S: AsyncWrite + Unpin> OsshStream<S> {
    pub fn new(inner: S, keyword: &str, padding_len: usize) -> Self {
        let mut seed = [0u8; SEED_LENGTH];
        rand::fill(&mut seed[..]);
        let c2s_key = derive_key(&seed, keyword.as_bytes(), b"client_to_server");
        let s2c_key = derive_key(&seed, keyword.as_bytes(), b"server_to_client");

        let mut preamble = BytesMut::with_capacity(SEED_LENGTH + 8 + padding_len);
        preamble.extend_from_slice(&seed);
        let encrypted_start = preamble.len();
        preamble.extend_from_slice(&MAGIC.to_be_bytes());
        preamble.extend_from_slice(&(padding_len as u32).to_be_bytes());
        let mut padding = vec![0u8; padding_len];
        rand::fill(&mut padding[..]);
        preamble.extend_from_slice(&padding);

        let mut c2s = Rc4::new(&c2s_key);
        c2s.apply(&mut preamble[encrypted_start..]);

        Self {
            inner,
            c2s,
            s2c: Rc4::new(&s2c_key),
            wstate: WriteState::Ident,
            wbuf: BytesMut::new(),
            wout: preamble,
            rstate: ReadState::Ident,
            rin: BytesMut::new(),
            rbuf: BytesMut::new(),
            rline: Vec::new(),
            kex_prefix: None,
        }
    }

    fn process_write(&mut self) {
        if let WriteState::Ident = self.wstate
            && let Some(index) = find_crlf(&self.wbuf)
        {
            let end = index + 2;
            let mut line = self.wbuf.split_to(end);
            self.c2s.apply(&mut line);
            self.wout.extend_from_slice(&line);
            self.wstate = WriteState::Kex;
        }

        if let WriteState::Kex = self.wstate {
            loop {
                if self.wbuf.len() < 5 {
                    break;
                }
                let packet_length = u32::from_be_bytes(self.wbuf[..4].try_into().unwrap()) as usize;
                if !(1..=MAX_PACKET).contains(&packet_length) {
                    break;
                }
                let total = packet_length + 4;
                if self.wbuf.len() < total {
                    break;
                }
                let mut packet = self.wbuf.split_to(total);
                let newkeys = packet[5] == MSG_NEWKEYS;
                self.c2s.apply(&mut packet);
                self.wout.extend_from_slice(&packet);
                if newkeys {
                    self.wstate = WriteState::Done;
                    break;
                }
            }
        }

        if let WriteState::Done = self.wstate
            && !self.wbuf.is_empty()
        {
            self.wout.extend_from_slice(&self.wbuf);
            self.wbuf.clear();
        }
    }

    fn process_read(&mut self) {
        loop {
            match self.rstate {
                ReadState::Ident => {
                    if self.rin.is_empty() {
                        break;
                    }
                    let mut byte = self.rin[0];
                    self.rin.advance(1);
                    self.s2c.apply(std::slice::from_mut(&mut byte));
                    self.rline.push(byte);
                    if self.rline.len() > MAX_LINE {
                        self.rline.clear();
                        continue;
                    }
                    if self.rline.ends_with(b"\r\n") {
                        if self.rline.starts_with(b"SSH-") {
                            self.rbuf.extend_from_slice(&self.rline);
                            self.rline.clear();
                            self.rstate = ReadState::Kex;
                            break;
                        }
                        self.rline.clear();
                    }
                }
                ReadState::Kex => {
                    if self.kex_prefix.is_none() {
                        if self.rin.len() < 5 {
                            break;
                        }
                        let mut prefix = self.rin.split_to(5);
                        self.s2c.apply(&mut prefix);
                        let packet_length =
                            u32::from_be_bytes(prefix[..4].try_into().unwrap()) as usize;
                        if !(1..=MAX_PACKET).contains(&packet_length) {
                            break;
                        }
                        self.kex_prefix = Some((prefix, packet_length - 1));
                    }
                    let remaining = self.kex_prefix.as_ref().unwrap().1;
                    if self.rin.len() < remaining {
                        break;
                    }
                    let (mut packet, _) = self.kex_prefix.take().unwrap();
                    let mut rest = self.rin.split_to(remaining);
                    self.s2c.apply(&mut rest);
                    packet.extend_from_slice(&rest);
                    let newkeys = packet.get(5) == Some(&MSG_NEWKEYS);
                    let normalized = normalize_packet(&packet);
                    self.rbuf.extend_from_slice(&normalized);
                    if newkeys {
                        self.rstate = ReadState::Done;
                        break;
                    }
                }
                ReadState::Done => break,
            }
        }
        if self.rin.is_empty() && self.rin.capacity() >= SHRINK_AT {
            self.rin = BytesMut::new();
        }
    }

    fn flush_out(&mut self, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        while !self.wout.is_empty() {
            match Pin::new(&mut self.inner).poll_write(cx, &self.wout) {
                Poll::Pending => return Poll::Pending,
                Poll::Ready(Ok(0)) => {
                    return Poll::Ready(Err(io::ErrorKind::WriteZero.into()));
                }
                Poll::Ready(Ok(n)) => {
                    self.wout.advance(n);
                }
                Poll::Ready(Err(err)) => return Poll::Ready(Err(err)),
            }
        }
        if self.wout.capacity() >= SHRINK_AT {
            self.wout = BytesMut::new();
        }
        Poll::Ready(Ok(()))
    }
}

impl<S: AsyncRead + AsyncWrite + Unpin> AsyncWrite for OsshStream<S> {
    fn poll_write(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<io::Result<usize>> {
        if self.flush_out(cx).is_pending() {
            return Poll::Pending;
        }
        self.wbuf.extend_from_slice(buf);
        self.process_write();
        match self.flush_out(cx) {
            Poll::Pending => {}
            Poll::Ready(Err(err)) => return Poll::Ready(Err(err)),
            Poll::Ready(Ok(())) => {}
        }
        Poll::Ready(Ok(buf.len()))
    }

    fn poll_flush(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        match self.flush_out(cx) {
            Poll::Pending => return Poll::Pending,
            Poll::Ready(Err(err)) => return Poll::Ready(Err(err)),
            Poll::Ready(Ok(())) => {}
        }
        Pin::new(&mut self.inner).poll_flush(cx)
    }

    fn poll_shutdown(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        match self.flush_out(cx) {
            Poll::Pending => return Poll::Pending,
            Poll::Ready(Err(err)) => return Poll::Ready(Err(err)),
            Poll::Ready(Ok(())) => {}
        }
        Pin::new(&mut self.inner).poll_shutdown(cx)
    }
}

impl<S: AsyncRead + AsyncWrite + Unpin> AsyncRead for OsshStream<S> {
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        loop {
            self.process_read();

            if !self.rbuf.is_empty() {
                let n = buf.remaining().min(self.rbuf.len());
                buf.put_slice(&self.rbuf[..n]);
                self.rbuf.advance(n);
                if self.rbuf.is_empty() && self.rbuf.capacity() >= SHRINK_AT {
                    self.rbuf = BytesMut::new();
                }
                return Poll::Ready(Ok(()));
            }

            if let ReadState::Done = self.rstate
                && !self.rin.is_empty()
            {
                let this = self.as_mut().get_mut();
                this.rbuf.extend_from_slice(&this.rin);
                this.rin.clear();
                continue;
            }

            let mut tmp = [0u8; 8192];
            let mut read_buf = ReadBuf::new(&mut tmp);
            match Pin::new(&mut self.inner).poll_read(cx, &mut read_buf) {
                Poll::Pending => return Poll::Pending,
                Poll::Ready(Err(err)) => return Poll::Ready(Err(err)),
                Poll::Ready(Ok(())) => {
                    let filled = read_buf.filled().len();
                    if filled == 0 {
                        return Poll::Ready(Ok(()));
                    }
                    self.rin.extend_from_slice(&tmp[..filled]);
                }
            }
        }
    }
}
