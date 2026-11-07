use btls::hash::MessageDigest;
use hmac::{Hmac, Mac};
use md5::Md5;
use sha1::Sha1;

use crate::openvpn::config::Auth;
use crate::openvpn::config::Cipher;

pub const MASTER_LEN: usize = 48;
const KEY_LEN: usize = 64;
const KEY2_LEN: usize = 4 * KEY_LEN;

type HmacSha1 = Hmac<Sha1>;
type HmacMd5 = Hmac<Md5>;

pub fn prf(secret: &[u8], label: &str, seed: &[&[u8]], len: usize) -> Vec<u8> {
    let mut label_seed = Vec::with_capacity(64);
    label_seed.extend_from_slice(label.as_bytes());
    for part in seed {
        label_seed.extend_from_slice(part);
    }

    let half = secret.len().div_ceil(2);
    let md5 = p_hash_md5(&secret[..half], &label_seed, len);
    let sha1 = p_hash_sha1(&secret[secret.len() - half..], &label_seed, len);
    md5.iter().zip(&sha1).map(|(a, b)| a ^ b).collect()
}

fn p_hash_md5(secret: &[u8], seed: &[u8], len: usize) -> Vec<u8> {
    p_hash::<HmacMd5>(secret, seed, len)
}

fn p_hash_sha1(secret: &[u8], seed: &[u8], len: usize) -> Vec<u8> {
    p_hash::<HmacSha1>(secret, seed, len)
}

fn p_hash<M>(secret: &[u8], seed: &[u8], len: usize) -> Vec<u8>
where
    M: Mac + hmac::digest::KeyInit,
{
    let mut out = Vec::with_capacity(len);
    let mut a = hmac::<M>(secret, seed);
    while out.len() < len {
        let mut block = Vec::with_capacity(a.len() + seed.len());
        block.extend_from_slice(&a);
        block.extend_from_slice(seed);
        out.extend_from_slice(&hmac::<M>(secret, &block));
        a = hmac::<M>(secret, &a);
    }
    out.truncate(len);
    out
}

fn hmac<M>(key: &[u8], data: &[u8]) -> Vec<u8>
where
    M: Mac + hmac::digest::KeyInit,
{
    let mut mac = <M as hmac::digest::KeyInit>::new_from_slice(key).expect("hmac key");
    mac.update(data);
    mac.finalize().into_bytes().to_vec()
}

#[derive(Clone)]
pub struct SeedData {
    pub pre_master: [u8; 48],
    pub client_random1: [u8; 32],
    pub server_random1: [u8; 32],
    pub client_random2: [u8; 32],
    pub server_random2: [u8; 32],
    pub client_sid: [u8; 8],
    pub server_sid: [u8; 8],
}

impl SeedData {
    pub fn as_seed(&self) -> Seed<'_> {
        Seed {
            pre_master: &self.pre_master,
            client_random1: &self.client_random1,
            server_random1: &self.server_random1,
            client_random2: &self.client_random2,
            server_random2: &self.server_random2,
            client_sid: &self.client_sid,
            server_sid: &self.server_sid,
        }
    }
}

pub struct Seed<'a> {
    pub pre_master: &'a [u8; 48],
    pub client_random1: &'a [u8; 32],
    pub server_random1: &'a [u8; 32],
    pub client_random2: &'a [u8; 32],
    pub server_random2: &'a [u8; 32],
    pub client_sid: &'a [u8; 8],
    pub server_sid: &'a [u8; 8],
}

pub struct KeyMaterial {
    pub encrypt: Vec<u8>,
    pub decrypt: Vec<u8>,
    pub mac_encrypt: Vec<u8>,
    pub mac_decrypt: Vec<u8>,
}

impl KeyMaterial {
    pub fn derive(seed: &Seed<'_>, cipher: Cipher, mac_len: usize) -> Self {
        let Seed {
            pre_master,
            client_random1,
            server_random1,
            client_random2,
            server_random2,
            client_sid,
            server_sid,
        } = *seed;
        let master = prf(
            pre_master,
            "OpenVPN master secret",
            &[client_random1, server_random1],
            MASTER_LEN,
        );
        let key_len = cipher.key_len();
        let expanded = prf(
            &master,
            "OpenVPN key expansion",
            &[client_random2, server_random2, client_sid, server_sid],
            KEY2_LEN,
        );

        let encrypt = expanded[..key_len].to_vec();
        let mac_encrypt = expanded[KEY_LEN..KEY_LEN + mac_len].to_vec();
        let decrypt = expanded[KEY_LEN + KEY_LEN..KEY_LEN + KEY_LEN + key_len].to_vec();
        let mac_decrypt =
            expanded[KEY_LEN + KEY_LEN + KEY_LEN..KEY_LEN + KEY_LEN + KEY_LEN + mac_len].to_vec();
        Self {
            encrypt,
            decrypt,
            mac_encrypt,
            mac_decrypt,
        }
    }
}

pub fn digest(auth: Auth) -> Option<MessageDigest> {
    match auth {
        Auth::Sha1 => Some(MessageDigest::sha1()),
        Auth::Sha256 => Some(MessageDigest::sha256()),
        Auth::Sha512 => Some(MessageDigest::sha512()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn prf_matches_openvpn_vector() {
        let secret = b"Lorem ipsum dolor sit amet, consectetur adipisici elit, sed eiusmod \
                       tempor incidunt ut labore et dolore magna aliqua.";
        let seed = b"Quis aute iure reprehenderit in voluptate velit esse cillum dolore";
        let expected = [
            0xd9, 0x8c, 0x85, 0x18, 0xc8, 0x5e, 0x94, 0x69, 0x27, 0x91, 0x6a, 0xcf, 0xc2, 0xd5,
            0x92, 0xfb, 0xb1, 0x56, 0x7e, 0x4b, 0x4b, 0x14, 0x59, 0xe6, 0xa9, 0x04, 0xac, 0x2d,
            0xda, 0xb7, 0x2d, 0x67,
        ];
        assert_eq!(prf(secret, "", &[seed], 32), expected);
    }

    #[test]
    fn prf_uses_split_secret() {
        let secret = [0x0bu8; 16];
        let label = "OpenVPN master secret";
        let seed = &[&[0u8, 1, 2, 3][..], &[4u8, 5, 6, 7][..]];
        let half = secret.len() / 2;
        let mut joined = label.as_bytes().to_vec();
        joined.extend_from_slice(&[0, 1, 2, 3, 4, 5, 6, 7]);
        let md5 = p_hash_md5(&secret[..half], &joined, 48);
        let sha1 = p_hash_sha1(&secret[secret.len() - half..], &joined, 48);
        let expected: Vec<u8> = md5.iter().zip(&sha1).map(|(a, b)| a ^ b).collect();
        assert_eq!(prf(&secret, label, seed, 48), expected);
        assert_ne!(
            prf(&secret, label, seed, 48),
            p_hash_sha1(&secret, &joined, 48)
        );
    }

    #[test]
    fn prf_is_deterministic() {
        let a = prf(b"k", "label", &[b"x", b"y"], 96);
        let b = prf(b"k", "label", &[b"x", b"y"], 96);
        assert_eq!(a, b);
        assert_ne!(a, prf(b"k", "label", &[b"x", b"z"], 96));
    }

    #[test]
    fn key_material_splits_sections() {
        let cipher = Cipher::Aes256Gcm;
        let seed = Seed {
            pre_master: &[1u8; 48],
            client_random1: &[2u8; 32],
            server_random1: &[3u8; 32],
            client_random2: &[4u8; 32],
            server_random2: &[5u8; 32],
            client_sid: &[6u8; 8],
            server_sid: &[7u8; 8],
        };
        let keys = KeyMaterial::derive(&seed, cipher, 0);
        assert_eq!(keys.encrypt.len(), 32);
        assert_eq!(keys.decrypt.len(), 32);
        assert!(keys.mac_encrypt.is_empty());
        assert_ne!(keys.encrypt, keys.decrypt);
    }
}
