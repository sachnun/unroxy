use std::sync::Arc;
use std::time::Duration;

use crate::openvpn::config::Profile;
use crate::openvpn::control::{Control, parse_push};
use crate::openvpn::error::Error;
use crate::openvpn::stack::{Stack, Stream};
use crate::openvpn::tls::CredentialStore;

#[derive(Debug, Clone)]
pub struct TunnelConfig {
    pub connect_timeout: Duration,
}

impl Default for TunnelConfig {
    fn default() -> Self {
        Self {
            connect_timeout: Duration::from_secs(20),
        }
    }
}

pub struct Tunnel {
    stack: Arc<Stack>,
    cipher: crate::openvpn::config::Cipher,
}

impl Tunnel {
    pub async fn connect(profile: &Profile, config: TunnelConfig) -> Result<Self, Error> {
        let _ = config.connect_timeout;
        let credentials = CredentialStore::new(&profile.credentials);
        let mut control = Control::connect(profile, &credentials).await?;
        let reply = control.push_request().await?;
        control.apply_push(&reply);
        control.install_keys().await?;
        let local = parse_push(&reply)
            .local
            .map(|address| {
                let [a, b, c, d] = address.octets();
                smoltcp::wire::Ipv4Address::new(a, b, c, d)
            })
            .unwrap_or(smoltcp::wire::Ipv4Address::new(10, 8, 0, 2));
        let cipher = control.cipher();
        let stack = Stack::spawn(control, local, profile.servers[0].protocol);
        let _ = config;
        Ok(Self { stack, cipher })
    }

    pub fn cipher(&self) -> crate::openvpn::config::Cipher {
        self.cipher
    }

    pub async fn open(&self, host: &str, port: u16) -> Result<Stream, Error> {
        self.stack.open(host, port).await
    }
}
