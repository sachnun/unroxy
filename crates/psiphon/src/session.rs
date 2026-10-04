use std::io;
use std::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::task::{Context, Poll};
use std::time::Duration;

use base64::Engine;
use bytes::Bytes;
use makiko::{
    AuthPasswordResult, ChannelConfig, Client, ClientConfig, ClientEvent, DisconnectError,
    GlobalReply, GlobalReq, TunnelStream,
};
use tokio::io::{AsyncRead, AsyncWrite, ReadBuf};
use tokio::sync::oneshot;

use crate::Error;
use crate::entry::Entry;
use crate::ossh::OsshStream;
use unroxy_net::TcpStream;

pub type Channel = TunnelStream;

pub struct Timeouts {
    pub connect: Duration,
    pub handshake: Duration,
    pub auth: Duration,
}

enum Transport {
    Plain(TcpStream),
    Obfuscated(Box<OsshStream<TcpStream>>),
}

impl AsyncRead for Transport {
    fn poll_read(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        match self.get_mut() {
            Transport::Plain(stream) => Pin::new(stream).poll_read(cx, buf),
            Transport::Obfuscated(stream) => Pin::new(stream.as_mut()).poll_read(cx, buf),
        }
    }
}

impl AsyncWrite for Transport {
    fn poll_write(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<io::Result<usize>> {
        match self.get_mut() {
            Transport::Plain(stream) => Pin::new(stream).poll_write(cx, buf),
            Transport::Obfuscated(stream) => Pin::new(stream.as_mut()).poll_write(cx, buf),
        }
    }

    fn poll_flush(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        match self.get_mut() {
            Transport::Plain(stream) => Pin::new(stream).poll_flush(cx),
            Transport::Obfuscated(stream) => Pin::new(stream.as_mut()).poll_flush(cx),
        }
    }

    fn poll_shutdown(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        match self.get_mut() {
            Transport::Plain(stream) => Pin::new(stream).poll_shutdown(cx),
            Transport::Obfuscated(stream) => Pin::new(stream.as_mut()).poll_shutdown(cx),
        }
    }
}

pub struct Session {
    pub ip: String,
    pub protocol: &'static str,
    client: Client,
    closed: Arc<AtomicBool>,
}

impl Session {
    pub async fn connect(
        entry: &Entry,
        timeouts: &Timeouts,
        sponsor_id: &str,
        propagation_channel_id: &str,
        client_platform: &str,
    ) -> Result<Arc<Session>, Error> {
        let expected = base64::engine::general_purpose::STANDARD
            .decode(&entry.ssh_host_key)
            .map_err(|err| Error::Core(err.to_string()))?;

        let obfuscated = !entry.ssh_obfuscated_key.is_empty() && entry.ossh_port != 0;
        let protocol = if obfuscated { "OSSH" } else { "SSH" };
        let port = if obfuscated {
            entry.ossh_port
        } else {
            entry.ssh_port
        };

        let tcp = match tokio::time::timeout(
            timeouts.connect,
            TcpStream::connect((entry.ip.as_str(), port)),
        )
        .await
        {
            Err(_) => return Err(Error::Timeout),
            Ok(Err(err)) => return Err(Error::Socket(err)),
            Ok(Ok(stream)) => stream,
        };
        tcp.set_nodelay(true).map_err(Error::Socket)?;

        let transport = if obfuscated {
            Transport::Obfuscated(Box::new(OsshStream::new(
                tcp,
                &entry.ssh_obfuscated_key,
                64,
            )))
        } else {
            Transport::Plain(tcp)
        };

        let (client, mut events, driver) = Client::open(transport, ClientConfig::default())
            .map_err(|err| Error::Core(err.to_string()))?;

        let closed = Arc::new(AtomicBool::new(false));

        {
            let closed = Arc::clone(&closed);
            tokio::spawn(async move {
                let _ = driver.await;
                closed.store(true, Ordering::Relaxed);
            });
        }

        {
            let closed = Arc::clone(&closed);
            tokio::spawn(async move {
                while let Ok(Some(event)) = events.recv().await {
                    if let ClientEvent::ServerPubkey(pubkey, accept) = event {
                        if pubkey.encode().as_ref() == expected.as_slice() {
                            accept.accept();
                        } else {
                            accept.reject(std::io::Error::other("unexpected host key"));
                        }
                    }
                }
                closed.store(true, Ordering::Relaxed);
            });
        }

        let session_id: String = (0..16)
            .map(|_| format!("{:02x}", rand::random::<u8>()))
            .collect();
        let auth_payload = serde_json::json!({
            "SessionId": session_id,
            "SshPassword": entry.ssh_password,
            "ClientCapabilities": ["server-requests"],
            "SponsorId": sponsor_id,
        })
        .to_string();

        let auth_deadline = timeouts.handshake.saturating_add(timeouts.auth);
        let auth = match tokio::time::timeout(
            auth_deadline,
            client.auth_password(entry.ssh_username.clone(), auth_payload),
        )
        .await
        {
            Err(_) => return Err(Error::Timeout),
            Ok(Err(err)) => return Err(Error::Core(err.to_string())),
            Ok(Ok(result)) => result,
        };
        if !matches!(auth, AuthPasswordResult::Success) {
            return Err(Error::Auth);
        }

        let handshake_payload = serde_json::json!({
            "propagation_channel_id": propagation_channel_id,
            "sponsor_id": sponsor_id,
            "client_version": "0",
            "client_platform": client_platform,
            "relay_protocol": protocol,
        })
        .to_string();

        let (reply_tx, reply_rx) = oneshot::channel();
        client
            .send_request(GlobalReq {
                request_type: "psiphon-handshake".to_string(),
                payload: Bytes::from(handshake_payload.into_bytes()),
                reply_tx: Some(reply_tx),
            })
            .map_err(|err| Error::Core(err.to_string()))?;

        match tokio::time::timeout(timeouts.handshake, reply_rx).await {
            Err(_) => return Err(Error::Timeout),
            Ok(Err(_)) => return Err(Error::Core("handshake aborted".into())),
            Ok(Ok(GlobalReply::Success(_))) => {}
            Ok(Ok(GlobalReply::Failure)) => return Err(Error::Core("handshake rejected".into())),
        }

        Ok(Arc::new(Session {
            ip: entry.ip.clone(),
            protocol,
            client,
            closed,
        }))
    }

    pub async fn dial(&self, host: &str, port: u16) -> Result<Channel, Error> {
        let (tunnel, receiver) = self
            .client
            .connect_tunnel(
                ChannelConfig::default(),
                (host.to_string(), port),
                ("127.0.0.1".to_string(), 0),
            )
            .await
            .map_err(|err| Error::Core(err.to_string()))?;
        Ok(TunnelStream::new(tunnel, receiver))
    }

    pub async fn is_closed(&self) -> bool {
        self.closed.load(Ordering::Relaxed)
    }

    pub async fn close(&self) {
        let _ = self.client.disconnect(DisconnectError::by_app());
        self.closed.store(true, Ordering::Relaxed);
    }
}

impl Drop for Session {
    fn drop(&mut self) {
        let _ = self.client.disconnect(DisconnectError::by_app());
    }
}
