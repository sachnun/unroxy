#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct PeerId(pub u32);

impl PeerId {
    pub const NONE: PeerId = PeerId(0);

    pub fn from_hex(value: &str) -> Option<Self> {
        let value = value.trim();
        let value = value
            .strip_prefix("0x")
            .or_else(|| value.strip_prefix("0X"))
            .unwrap_or(value);
        u32::from_str_radix(value, 16).ok().map(PeerId)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn peer_id_parses_hex() {
        assert_eq!(PeerId::from_hex("1a2b3c4d"), Some(PeerId(0x1a2b3c4d)));
        assert_eq!(PeerId::from_hex("0x0A"), Some(PeerId(10)));
        assert_eq!(PeerId::from_hex("zz"), None);
    }
}
