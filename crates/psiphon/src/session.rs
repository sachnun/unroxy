use std::sync::Arc;
use std::time::Duration;

use base64::Engine;
use russh::client::{self, Config as SshConfig, Handle};
use russh::keys::PublicKeyOrCertificate;
use tokio::net::TcpStream;
use tokio::sync::Mutex;

use crate::Error;
use crate::entry::Entry;
use crate::ossh::OsshStream;

pub type Channel = russh::ChannelStream<client::Msg>;

pub struct Timeouts {
    pub connect: Duration,
    pub handshake: Duration,
    pub auth: Duration,
}

struct Handler {
    expected: Vec<u8>,
}

impl client::Handler for Handler {
    type Error = russh::Error;

    async fn check_server_key(
        &mut self,
        key: &PublicKeyOrCertificate,
    ) -> Result<bool, Self::Error> {
        Ok(key.public_key().to_bytes().unwrap_or_default() == self.expected)
    }
}

pub struct Session {
    pub ip: String,
    pub protocol: &'static str,
    handle: Mutex<Handle<Handler>>,
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

        let config = Arc::new(SshConfig {
            client_id: russh::SshId::Standard("SSH-2.0-OpenSSH_9.6".into()),
            ..Default::default()
        });

        let connected = async {
            if obfuscated {
                let stream = OsshStream::new(tcp, &entry.ssh_obfuscated_key, 64);
                client::connect_stream(config, stream, Handler { expected }).await
            } else {
                client::connect_stream(config, tcp, Handler { expected }).await
            }
        };

        let mut handle = match tokio::time::timeout(timeouts.handshake, connected).await {
            Err(_) => return Err(Error::Timeout),
            Ok(Err(err)) => return Err(Error::Core(err.to_string())),
            Ok(Ok(handle)) => handle,
        };

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

        let auth = match tokio::time::timeout(
            timeouts.auth,
            handle.authenticate_password(entry.ssh_username.clone(), auth_payload),
        )
        .await
        {
            Err(_) => return Err(Error::Timeout),
            Ok(Err(err)) => return Err(Error::Core(err.to_string())),
            Ok(Ok(auth)) => auth,
        };
        if !auth.success() {
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

        let response = match tokio::time::timeout(
            timeouts.handshake,
            handle.send_request("psiphon-handshake", true, handshake_payload.into_bytes()),
        )
        .await
        {
            Err(_) => return Err(Error::Timeout),
            Ok(Err(err)) => return Err(Error::Core(err.to_string())),
            Ok(Ok(response)) => response,
        };
        if response.is_none() {
            return Err(Error::Core("handshake rejected".into()));
        }

        Ok(Arc::new(Session {
            ip: entry.ip.clone(),
            protocol,
            handle: Mutex::new(handle),
        }))
    }

    pub async fn dial(&self, host: &str, port: u16) -> Result<Channel, Error> {
        let handle = self.handle.lock().await;
        let channel = handle
            .channel_open_direct_tcpip(host.to_string(), port as u32, "127.0.0.1".to_string(), 0)
            .await
            .map_err(|err| Error::Core(err.to_string()))?;
        drop(handle);
        Ok(channel.into_stream())
    }

    pub async fn is_closed(&self) -> bool {
        self.handle.lock().await.is_closed()
    }

    pub async fn close(&self) {
        let handle = self.handle.lock().await;
        let _ = handle
            .disconnect(russh::Disconnect::ByApplication, "", "")
            .await;
    }
}
