use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};

use crate::openvpn::error::Error;

const MAX_PACKET: usize = 65_535;

pub async fn read_packet<S: AsyncRead + Unpin>(stream: &mut S) -> Result<Vec<u8>, Error> {
    let mut length = [0u8; 2];
    stream.read_exact(&mut length).await?;
    let size = u16::from_be_bytes(length) as usize;
    if size == 0 {
        return Err(Error::Transport("empty frame".into()));
    }
    let mut payload = vec![0u8; size];
    stream.read_exact(&mut payload).await?;
    Ok(payload)
}

pub async fn write_packet<S: AsyncWrite + Unpin>(
    stream: &mut S,
    payload: &[u8],
) -> Result<(), Error> {
    if payload.len() > MAX_PACKET {
        return Err(Error::Transport("packet too large".into()));
    }
    let mut frame = Vec::with_capacity(2 + payload.len());
    frame.extend_from_slice(&(payload.len() as u16).to_be_bytes());
    frame.extend_from_slice(payload);
    stream.write_all(&frame).await?;
    stream.flush().await?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn roundtrip_length_prefix() {
        let (mut client, mut server) = tokio::io::duplex(1024);
        write_packet(&mut client, b"hello").await.expect("write");
        assert_eq!(read_packet(&mut server).await.expect("read"), b"hello");
    }

    #[tokio::test]
    async fn rejects_empty_frame() {
        let (mut client, mut server) = tokio::io::duplex(64);
        client.write_all(&[0u8, 0]).await.expect("write");
        assert!(read_packet(&mut server).await.is_err());
    }

    #[tokio::test]
    async fn rejects_truncated_frame() {
        let (mut client, mut server) = tokio::io::duplex(64);
        client.write_all(&[0u8, 8, 1, 2]).await.expect("write");
        drop(client);
        assert!(read_packet(&mut server).await.is_err());
    }

    #[tokio::test]
    async fn rejects_oversized_write() {
        let (mut client, _server) = tokio::io::duplex(64);
        let payload = vec![0u8; MAX_PACKET + 1];
        let error = write_packet(&mut client, &payload)
            .await
            .expect_err("oversized");
        assert!(error.to_string().contains("too large"));
    }
}
