use std::io::Write;
use std::sync::Arc;

use base64::Engine;
use russh::keys::{Algorithm, PrivateKey};
use russh::server::{Auth, ChannelOpenHandle, Handler, Msg, Session};
use russh::{Channel, ChannelId};

struct Mock;

impl Handler for Mock {
    type Error = russh::Error;

    async fn auth_password(&mut self, _user: &str, _password: &str) -> Result<Auth, Self::Error> {
        Ok(Auth::Accept)
    }

    async fn channel_open_session(
        &mut self,
        _channel: Channel<Msg>,
        reply: ChannelOpenHandle,
        _session: &mut Session,
    ) -> Result<(), Self::Error> {
        reply.accept().await;
        Ok(())
    }

    async fn channel_open_direct_tcpip(
        &mut self,
        _channel: Channel<Msg>,
        _host: &str,
        _port: u32,
        _originator_address: &str,
        _originator_port: u32,
        reply: ChannelOpenHandle,
        _session: &mut Session,
    ) -> Result<(), Self::Error> {
        reply.accept().await;
        Ok(())
    }

    async fn data(
        &mut self,
        channel: ChannelId,
        data: &[u8],
        session: &mut Session,
    ) -> Result<(), Self::Error> {
        session.data(channel, data.to_vec())?;
        Ok(())
    }

    async fn global_request(
        &mut self,
        _name: &str,
        _session: &mut Session,
    ) -> Result<bool, Self::Error> {
        Ok(true)
    }
}

#[tokio::main]
async fn main() {
    let key = PrivateKey::random(&mut rand010::rng(), Algorithm::Ed25519).expect("host key");
    let host_key = base64::engine::general_purpose::STANDARD
        .encode(key.public_key().to_bytes().expect("host key bytes"));

    let mut config = russh::server::Config::default();
    config.keys.push(key);
    config.inactivity_timeout = None;
    let config = Arc::new(config);

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind");
    println!("{} {host_key}", listener.local_addr().expect("addr").port());
    let _ = std::io::stdout().flush();

    loop {
        let Ok((socket, _)) = listener.accept().await else {
            return;
        };
        let config = Arc::clone(&config);
        tokio::spawn(async move {
            if let Ok(running) = russh::server::run_stream(config, socket, Mock).await {
                let _ = running.await;
            }
        });
    }
}
