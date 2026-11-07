use aes::Aes128;
use aes::Aes256;
use aes_gcm::aead::{Aead, KeyInit, Payload};
use aes_gcm::{Aes128Gcm, Aes256Gcm};
use btls::hash::MessageDigest;
use btls::hmac::Hmac as BtlsHmac;
use cbc::cipher::block_padding::Pkcs7;
use cbc::cipher::{BlockDecryptMut, BlockEncryptMut, KeyIvInit};

use super::keys::digest;
use crate::openvpn::config::{Auth, Cipher};
use crate::openvpn::error::Error;

const TAG_LEN: usize = 16;

type Cbc128 = cbc::Encryptor<Aes128>;
type Cbc256 = cbc::Encryptor<Aes256>;
type Dec128 = cbc::Decryptor<Aes128>;
type Dec256 = cbc::Decryptor<Aes256>;

pub struct DataCipher {
    cipher: Cipher,
    auth: Auth,
    digest: MessageDigest,
    key: Vec<u8>,
    mac: Vec<u8>,
    implicit_iv: Vec<u8>,
}

impl DataCipher {
    pub fn new(cipher: Cipher, auth: Auth, key: &[u8], mac: &[u8]) -> Result<Self, Error> {
        if key.len() < cipher.key_len() {
            return Err(Error::Data("short key".into()));
        }
        if !cipher.is_aead() && mac.len() < auth.size() {
            return Err(Error::Data("short mac key".into()));
        }
        let digest = digest(auth).ok_or_else(|| Error::Data("unknown digest".into()))?;
        let implicit_iv = if cipher.is_aead() {
            let mut iv = vec![0u8; cipher.iv_len() - 4];
            let take = iv.len().min(mac.len());
            iv[..take].copy_from_slice(&mac[..take]);
            iv
        } else {
            Vec::new()
        };
        Ok(Self {
            cipher,
            auth,
            digest,
            key: key[..cipher.key_len()].to_vec(),
            mac: mac.to_vec(),
            implicit_iv,
        })
    }

    pub fn encrypt(
        &self,
        nonce: &[u8],
        packet_id: u32,
        plaintext: &[u8],
    ) -> Result<Vec<u8>, Error> {
        if self.cipher.is_aead() {
            return self.encrypt_aead(nonce, packet_id, plaintext);
        }
        self.encrypt_cbc(packet_id, plaintext)
    }

    pub fn decrypt(&self, header: &[u8], body: &[u8]) -> Result<(u32, Vec<u8>), Error> {
        if self.cipher.is_aead() {
            return self.decrypt_aead(header, body);
        }
        self.decrypt_cbc(body)
    }

    fn aead(&self) -> Result<AeadKind, Error> {
        match self.cipher {
            Cipher::Aes256Gcm => Ok(AeadKind::Aes256(Box::new(
                Aes256Gcm::new_from_slice(&self.key).map_err(|_| Error::Data("gcm key".into()))?,
            ))),
            Cipher::Aes128Gcm => Ok(AeadKind::Aes128(Box::new(
                Aes128Gcm::new_from_slice(&self.key).map_err(|_| Error::Data("gcm key".into()))?,
            ))),
            _ => Err(Error::Data("not an aead cipher".into())),
        }
    }

    fn encrypt_aead(
        &self,
        header: &[u8],
        packet_id: u32,
        plaintext: &[u8],
    ) -> Result<Vec<u8>, Error> {
        let iv = aead_nonce(&self.implicit_iv, packet_id);
        let payload = Payload {
            msg: plaintext,
            aad: header,
        };
        let sealed: Vec<u8> = match self.aead()? {
            AeadKind::Aes128(cipher) => cipher
                .encrypt(aes_gcm::Nonce::from_slice(&iv), payload)
                .map_err(|_| Error::Data("gcm encrypt".into())),
            AeadKind::Aes256(cipher) => cipher
                .encrypt(aes_gcm::Nonce::from_slice(&iv), payload)
                .map_err(|_| Error::Data("gcm encrypt".into())),
        }?;
        Ok(tag_first(&sealed, TAG_LEN))
    }

    fn decrypt_aead(&self, header: &[u8], body: &[u8]) -> Result<(u32, Vec<u8>), Error> {
        if body.len() < TAG_LEN {
            return Err(Error::Data("short gcm packet".into()));
        }
        let packet_id =
            u32::from_be_bytes(header[header.len() - 4..].try_into().expect("packet id"));
        let sealed = tag_last(body);
        let iv = aead_nonce(&self.implicit_iv, packet_id);
        let payload = Payload {
            msg: &sealed,
            aad: header,
        };
        let plaintext: Vec<u8> = match self.aead()? {
            AeadKind::Aes128(cipher) => cipher
                .decrypt(aes_gcm::Nonce::from_slice(&iv), payload)
                .map_err(|_| Error::Data("gcm decrypt".into())),
            AeadKind::Aes256(cipher) => cipher
                .decrypt(aes_gcm::Nonce::from_slice(&iv), payload)
                .map_err(|_| Error::Data("gcm decrypt".into())),
        }?;
        Ok((packet_id, plaintext))
    }

    fn encrypt_cbc(&self, packet_id: u32, plaintext: &[u8]) -> Result<Vec<u8>, Error> {
        let mut iv = [0u8; 16];
        rand::fill(&mut iv);

        let mut input = Vec::with_capacity(4 + plaintext.len());
        input.extend_from_slice(&packet_id.to_be_bytes());
        input.extend_from_slice(plaintext);

        let ciphertext = match self.cipher {
            Cipher::Aes256Cbc => Cbc256::new_from_slices(&self.key, &iv)
                .map_err(|_| Error::Data("cbc key".into()))?
                .encrypt_padded_vec_mut::<Pkcs7>(&input),
            _ => Cbc128::new_from_slices(&self.key, &iv)
                .map_err(|_| Error::Data("cbc key".into()))?
                .encrypt_padded_vec_mut::<Pkcs7>(&input),
        };

        let mut mac = BtlsHmac::init(&self.mac[..self.auth.size()], &self.digest)
            .map_err(|_| Error::Data("hmac init".into()))?;
        mac.update(&iv).map_err(|_| Error::Data("hmac".into()))?;
        mac.update(&ciphertext)
            .map_err(|_| Error::Data("hmac".into()))?;
        let tag = mac.finalize().map_err(|_| Error::Data("hmac".into()))?;

        let mut packet = Vec::with_capacity(tag.len() + 16 + ciphertext.len());
        packet.extend_from_slice(&tag);
        packet.extend_from_slice(&iv);
        packet.extend_from_slice(&ciphertext);
        Ok(packet)
    }

    fn decrypt_cbc(&self, body: &[u8]) -> Result<(u32, Vec<u8>), Error> {
        let mac_len = self.auth.size();
        if body.len() < mac_len + 16 + 16 {
            return Err(Error::Data("short cbc packet".into()));
        }
        let (tag, rest) = body.split_at(mac_len);
        let (iv, ciphertext) = rest.split_at(16);

        let mut mac = BtlsHmac::init(&self.mac[..mac_len], &self.digest)
            .map_err(|_| Error::Data("hmac init".into()))?;
        mac.update(iv).map_err(|_| Error::Data("hmac".into()))?;
        mac.update(ciphertext)
            .map_err(|_| Error::Data("hmac".into()))?;
        let expected = mac.finalize().map_err(|_| Error::Data("hmac".into()))?;
        if !constant_time_eq(&expected, tag) {
            return Err(Error::Data("hmac mismatch".into()));
        }

        let plaintext = match self.cipher {
            Cipher::Aes256Cbc => Dec256::new_from_slices(&self.key, iv)
                .map_err(|_| Error::Data("cbc key".into()))?
                .decrypt_padded_vec_mut::<Pkcs7>(ciphertext)
                .map_err(|_| Error::Data("cbc padding".into()))?,
            _ => Dec128::new_from_slices(&self.key, iv)
                .map_err(|_| Error::Data("cbc key".into()))?
                .decrypt_padded_vec_mut::<Pkcs7>(ciphertext)
                .map_err(|_| Error::Data("cbc padding".into()))?,
        };

        if plaintext.len() < 4 {
            return Err(Error::Data("short cbc payload".into()));
        }
        let packet_id = u32::from_be_bytes(plaintext[..4].try_into().expect("packet id"));
        Ok((packet_id, plaintext[4..].to_vec()))
    }
}

fn aead_nonce(implicit_iv: &[u8], packet_id: u32) -> [u8; 12] {
    let mut iv = [0u8; 12];
    iv[..4].copy_from_slice(&packet_id.to_be_bytes());
    let take = implicit_iv.len().min(8);
    iv[4..4 + take].copy_from_slice(&implicit_iv[..take]);
    iv
}

fn tag_first(sealed: &[u8], tag_len: usize) -> Vec<u8> {
    let split = sealed.len() - tag_len;
    let mut out = Vec::with_capacity(sealed.len());
    out.extend_from_slice(&sealed[split..]);
    out.extend_from_slice(&sealed[..split]);
    out
}

fn tag_last(body: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(body.len());
    out.extend_from_slice(&body[TAG_LEN..]);
    out.extend_from_slice(&body[..TAG_LEN]);
    out
}

fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    let mut diff = 0u8;
    for (x, y) in a.iter().zip(b) {
        diff |= x ^ y;
    }
    diff == 0
}

enum AeadKind {
    Aes128(Box<Aes128Gcm>),
    Aes256(Box<Aes256Gcm>),
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

    fn key(len: usize, seed: u8) -> Vec<u8> {
        (0..len).map(|i| seed.wrapping_add(i as u8)).collect()
    }

    #[test]
    fn gcm_roundtrip_with_packet_id_in_aad() {
        let cipher =
            DataCipher::new(Cipher::Aes128Gcm, Auth::Sha1, &key(16, 0), &key(20, 100)).unwrap();
        let mut header = hex("481a2b3c");
        header.extend_from_slice(&7u32.to_be_bytes());
        let body = cipher
            .encrypt(&header, 7, b"openvpn data channel payload")
            .unwrap();
        let (packet_id, plaintext) = cipher.decrypt(&header, &body).expect("decrypt");
        assert_eq!(packet_id, 7);
        assert_eq!(plaintext, b"openvpn data channel payload");
    }

    #[test]
    fn cbc_matches_openvpn_layout() {
        let cipher =
            DataCipher::new(Cipher::Aes128Cbc, Auth::Sha1, &key(16, 0), &key(20, 100)).unwrap();
        let body = hex(
            "c0d7f47572bbe4cf58d4d36b837db46e128e852ec8c9cacbcccdcecfd0d1d2d3d4d5d6d77c31c1bd3ba8c787a033e04855358bf1a3b440e9ba25de1c0c07509e234d1baf47a5f8af962a41bf351b35d11b0e00fb",
        );
        let (packet_id, plaintext) = cipher.decrypt(&[], &body).expect("decrypt");
        assert_eq!(packet_id, 0x01020304);
        assert_eq!(plaintext, b"openvpn data channel payload");
    }

    #[test]
    fn gcm_roundtrip() {
        let cipher =
            DataCipher::new(Cipher::Aes256Gcm, Auth::Sha1, &key(32, 7), &key(20, 9)).unwrap();
        let mut header = hex("481a2b3c");
        header.extend_from_slice(&42u32.to_be_bytes());
        let packet = cipher.encrypt(&header, 42, b"hello openvpn world").unwrap();
        let (packet_id, out) = cipher.decrypt(&header, &packet).unwrap();
        assert_eq!(packet_id, 42);
        assert_eq!(out, b"hello openvpn world");
    }

    #[test]
    fn cbc_roundtrip() {
        let cipher =
            DataCipher::new(Cipher::Aes128Cbc, Auth::Sha1, &key(16, 3), &key(20, 5)).unwrap();
        let packet = cipher.encrypt(&[], 7, b"cbc payload with padding").unwrap();
        let (packet_id, out) = cipher.decrypt(&[], &packet).unwrap();
        assert_eq!(packet_id, 7);
        assert_eq!(out, b"cbc payload with padding");
    }

    #[test]
    fn cbc_rejects_tampering() {
        let cipher =
            DataCipher::new(Cipher::Aes256Cbc, Auth::Sha256, &key(32, 1), &key(32, 2)).unwrap();
        let mut packet = cipher.encrypt(&[], 1, b"payload").unwrap();
        let last = packet.len() - 1;
        packet[last] ^= 0xff;
        assert!(cipher.decrypt(&[], &packet).is_err());
    }

    #[test]
    fn gcm_rejects_tampering() {
        let cipher =
            DataCipher::new(Cipher::Aes128Gcm, Auth::Sha1, &key(16, 4), &key(20, 6)).unwrap();
        let header = hex("481a2b3c");
        let mut packet = cipher.encrypt(&header, 3, b"payload").unwrap();
        packet[0] ^= 0x01;
        assert!(cipher.decrypt(&header, &packet).is_err());
        let mut other = cipher.encrypt(&header, 3, b"payload").unwrap();
        let len = other.len();
        other[len - 1] ^= 0x01;
        assert!(cipher.decrypt(&header, &other).is_err());
    }

    #[test]
    fn gcm_authenticates_header() {
        let cipher =
            DataCipher::new(Cipher::Aes128Gcm, Auth::Sha1, &key(16, 4), &key(20, 6)).unwrap();
        let packet = cipher.encrypt(&hex("481a2b3c"), 3, b"payload").unwrap();
        assert!(cipher.decrypt(&hex("481a2b3d"), &packet).is_err());
    }
}
