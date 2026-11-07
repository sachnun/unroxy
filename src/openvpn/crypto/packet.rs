#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct PeerId(pub u32);

impl PeerId {
    pub const NONE: PeerId = PeerId(0);
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn peer_id_encodes_three_bytes() {
        let bytes = PeerId(0x010203).0.to_be_bytes();
        assert_eq!(&bytes[1..], &[1, 2, 3]);
    }
}
