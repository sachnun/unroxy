use std::collections::VecDeque;
use std::io::{Read, Write};

use btls::ssl::{
    HandshakeError, MidHandshakeSslStream, Ssl, SslConnector, SslContext, SslMethod, SslStream,
    SslStreamBuilder, SslVerifyMode,
};
use btls::x509::X509;
use btls::x509::store::X509StoreBuilder;

use crate::openvpn::config::Credentials;
use crate::openvpn::error::Error;

pub struct CredentialStore {
    cert: Vec<u8>,
    key: Vec<u8>,
    ca: Vec<u8>,
}

impl CredentialStore {
    pub fn new(credentials: &Credentials) -> Self {
        Self {
            cert: credentials.cert.as_bytes().to_vec(),
            key: credentials.key.as_bytes().to_vec(),
            ca: credentials.ca.as_bytes().to_vec(),
        }
    }

    pub fn context(&self, host: &str) -> Result<SslContext, Error> {
        let mut builder = SslConnector::builder(SslMethod::tls())
            .map_err(|err| Error::Tls(format!("{err:?}")))?;
        builder.set_verify(SslVerifyMode::PEER);
        let mut store = X509StoreBuilder::new().map_err(|err| Error::Tls(format!("{err:?}")))?;
        let anchors =
            X509::stack_from_pem(&self.ca).map_err(|err| Error::Tls(format!("{err:?}")))?;
        for anchor in &anchors {
            store
                .add_cert(anchor)
                .map_err(|err| Error::Tls(format!("{err:?}")))?;
        }
        builder.set_cert_store(store.build());
        let cert = X509::from_pem(&self.cert).map_err(|err| Error::Tls(format!("{err:?}")))?;
        let key = btls::pkey::PKey::private_key_from_pem(&self.key)
            .map_err(|err| Error::Tls(format!("{err:?}")))?;
        builder
            .set_certificate(&cert)
            .map_err(|err| Error::Tls(format!("{err:?}")))?;
        builder
            .set_private_key(&key)
            .map_err(|err| Error::Tls(format!("{err:?}")))?;
        builder
            .check_private_key()
            .map_err(|err| Error::Tls(format!("{err:?}")))?;
        let _ = host;
        Ok(builder.build().into_context())
    }
}

#[derive(Default)]
pub struct MemoryStream {
    inbound: VecDeque<u8>,
    outbound: VecDeque<u8>,
}

impl MemoryStream {
    pub fn feed(&mut self, data: &[u8]) {
        self.inbound.extend(data);
    }

    pub fn pending_inbound(&self) -> usize {
        self.inbound.len()
    }

    pub fn take_outbound(&mut self) -> Vec<u8> {
        self.outbound.drain(..).collect()
    }
}

impl Read for MemoryStream {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        if self.inbound.is_empty() {
            return Err(std::io::Error::from(std::io::ErrorKind::WouldBlock));
        }
        let len = buf.len().min(self.inbound.len());
        for slot in buf.iter_mut().take(len) {
            *slot = self.inbound.pop_front().expect("inbound");
        }
        Ok(len)
    }
}

impl Write for MemoryStream {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        self.outbound.extend(buf);
        Ok(buf.len())
    }

    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

pub struct TlsState {
    handshake: Option<MidHandshakeSslStream<MemoryStream>>,
    stream: Option<SslStream<MemoryStream>>,
    plaintext: Vec<u8>,
    offset: usize,
    pending: Vec<u8>,
}

impl TlsState {
    pub fn start(context: &SslContext, host: &str) -> Result<Self, Error> {
        let mut ssl = Ssl::new(context).map_err(|err| Error::Tls(format!("{err:?}")))?;
        ssl.set_hostname(host)
            .map_err(|err| Error::Tls(format!("{err:?}")))?;
        let mid = SslStreamBuilder::new(ssl, MemoryStream::default()).setup_connect();
        Ok(Self {
            handshake: Some(mid),
            stream: None,
            plaintext: Vec::new(),
            offset: 0,
            pending: Vec::new(),
        })
    }

    pub fn is_ready(&self) -> bool {
        self.stream.is_some()
    }

    pub fn feed(&mut self, ciphertext: &[u8]) -> Result<(), Error> {
        if let Some(mid) = self.handshake.as_mut() {
            mid.get_mut().feed(ciphertext);
            return Ok(());
        }
        if let Some(stream) = self.stream.as_mut() {
            stream.get_mut().feed(ciphertext);
        }
        if self.stream.is_some() {
            self.pump()?;
            return Ok(());
        }
        Err(Error::Tls("tls closed".into()))
    }

    pub fn drive(&mut self) -> Result<(), Error> {
        if let Some(mid) = self.handshake.take() {
            match mid.handshake() {
                Ok(stream) => {
                    self.stream = Some(stream);
                }
                Err(HandshakeError::WouldBlock(next)) => {
                    self.handshake = Some(next);
                    return Ok(());
                }
                Err(HandshakeError::Failure(next)) => {
                    return Err(Error::Tls(format!("{:?}", next.error())));
                }
                Err(HandshakeError::SetupFailure(err)) => {
                    return Err(Error::Tls(format!("{err:?}")));
                }
            }
        }
        self.pump()
    }

    pub fn pump(&mut self) -> Result<(), Error> {
        let Some(stream) = self.stream.as_mut() else {
            return Ok(());
        };
        let mut buffer = [0u8; 16 * 1024];
        loop {
            if stream.ssl().pending() == 0 && stream.get_ref().pending_inbound() == 0 {
                break;
            }
            match stream.ssl_read(&mut buffer) {
                Ok(0) => {
                    break;
                }
                Ok(n) => {
                    self.plaintext.extend_from_slice(&buffer[..n]);
                }
                Err(_) => break,
            }
        }
        Ok(())
    }

    pub fn take_outbound(&mut self) -> Vec<u8> {
        if let Some(mid) = self.handshake.as_mut() {
            return mid.get_mut().take_outbound();
        }
        if let Some(stream) = self.stream.as_mut() {
            return stream.get_mut().take_outbound();
        }
        Vec::new()
    }

    pub fn take_plaintext(&mut self) -> Option<Vec<u8>> {
        if self.offset >= self.plaintext.len() {
            self.plaintext.clear();
            self.offset = 0;
            return None;
        }
        let chunk = self.plaintext[self.offset..].to_vec();
        self.plaintext.clear();
        self.offset = 0;
        Some(chunk)
    }

    pub fn write(&mut self, data: &[u8]) -> Result<(), Error> {
        if self.stream.is_none() {
            return Err(Error::Tls("handshake incomplete".into()));
        }
        self.pending.extend_from_slice(data);
        self.drain_pending()
    }

    fn drain_pending(&mut self) -> Result<(), Error> {
        loop {
            if self.pending.is_empty() {
                return Ok(());
            }
            let Some(stream) = self.stream.as_mut() else {
                return Err(Error::Tls("handshake incomplete".into()));
            };
            match stream.ssl_write(&self.pending) {
                Ok(0) => return Ok(()),
                Ok(written) => {
                    self.pending.drain(..written);
                }
                Err(_err) => {
                    return Ok(());
                }
            }
        }
    }
}
