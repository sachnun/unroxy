use std::fmt;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Protocol {
    Udp,
    Tcp,
}

impl Protocol {
    pub fn as_str(self) -> &'static str {
        match self {
            Protocol::Udp => "udp",
            Protocol::Tcp => "tcp",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Auth {
    Sha1,
    Sha256,
    Sha512,
}

impl Auth {
    pub fn parse(name: &str) -> Option<Self> {
        match name.trim().to_ascii_uppercase().as_str() {
            "SHA1" => Some(Auth::Sha1),
            "SHA256" => Some(Auth::Sha256),
            "SHA512" => Some(Auth::Sha512),
            _ => None,
        }
    }

    pub fn size(self) -> usize {
        match self {
            Auth::Sha1 => 20,
            Auth::Sha256 => 32,
            Auth::Sha512 => 64,
        }
    }

    pub fn as_str(self) -> &'static str {
        match self {
            Auth::Sha1 => "SHA1",
            Auth::Sha256 => "SHA256",
            Auth::Sha512 => "SHA512",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Cipher {
    Aes256Gcm,
    Aes128Gcm,
    Aes256Cbc,
    Aes128Cbc,
}

impl Cipher {
    pub fn parse(name: &str) -> Option<Self> {
        match name.trim().to_ascii_uppercase().as_str() {
            "AES-256-GCM" => Some(Cipher::Aes256Gcm),
            "AES-128-GCM" => Some(Cipher::Aes128Gcm),
            "AES-256-CBC" => Some(Cipher::Aes256Cbc),
            "AES-128-CBC" => Some(Cipher::Aes128Cbc),
            _ => None,
        }
    }

    pub fn is_aead(self) -> bool {
        matches!(self, Cipher::Aes256Gcm | Cipher::Aes128Gcm)
    }

    pub fn key_len(self) -> usize {
        match self {
            Cipher::Aes256Gcm | Cipher::Aes256Cbc => 32,
            Cipher::Aes128Gcm | Cipher::Aes128Cbc => 16,
        }
    }

    pub fn iv_len(self) -> usize {
        if self.is_aead() { 12 } else { 16 }
    }
}

impl fmt::Display for Cipher {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Cipher::Aes256Gcm => "AES-256-GCM",
            Cipher::Aes128Gcm => "AES-128-GCM",
            Cipher::Aes256Cbc => "AES-256-CBC",
            Cipher::Aes128Cbc => "AES-128-CBC",
        })
    }
}

#[derive(Debug, Clone)]
pub struct Server {
    pub host: String,
    pub port: u16,
    pub protocol: Protocol,
}

#[derive(Clone, Default)]
pub struct Credentials {
    pub ca: String,
    pub cert: String,
    pub key: String,
}

impl fmt::Debug for Credentials {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Credentials")
            .field("ca", &self.ca.len())
            .field("cert", &self.cert.len())
            .field("key", &self.key.len())
            .finish()
    }
}

#[derive(Debug, Clone)]
pub struct Profile {
    pub servers: Vec<Server>,
    pub cipher: Cipher,
    pub auth: Auth,
    pub credentials: Credentials,
}
