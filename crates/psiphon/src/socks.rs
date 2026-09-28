use std::sync::Arc;

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};

use crate::Inner;

pub async fn serve(listener: TcpListener, inner: Arc<Inner>) {
    loop {
        let Ok((client, _)) = listener.accept().await else {
            return;
        };
        let inner = Arc::clone(&inner);
        tokio::spawn(async move {
            let _ = handle(client, inner).await;
        });
    }
}

async fn handle(mut client: TcpStream, inner: Arc<Inner>) -> std::io::Result<()> {
    let mut greeting = [0u8; 2];
    client.read_exact(&mut greeting).await?;
    if greeting[0] != 0x05 {
        return Ok(());
    }
    let mut methods = vec![0u8; greeting[1] as usize];
    client.read_exact(&mut methods).await?;
    client.write_all(&[0x05, 0x00]).await?;

    let mut request = [0u8; 4];
    client.read_exact(&mut request).await?;
    if request[0] != 0x05 || request[1] != 0x01 {
        return Ok(());
    }

    let host = match request[3] {
        0x01 => {
            let mut address = [0u8; 4];
            client.read_exact(&mut address).await?;
            std::net::Ipv4Addr::from(address).to_string()
        }
        0x04 => {
            let mut address = [0u8; 16];
            client.read_exact(&mut address).await?;
            std::net::Ipv6Addr::from(address).to_string()
        }
        0x03 => {
            let mut length = [0u8; 1];
            client.read_exact(&mut length).await?;
            let mut address = vec![0u8; length[0] as usize];
            client.read_exact(&mut address).await?;
            String::from_utf8_lossy(&address).into_owned()
        }
        _ => return Ok(()),
    };

    let mut port = [0u8; 2];
    client.read_exact(&mut port).await?;
    let port = u16::from_be_bytes(port);

    let Some(session) = inner.pick_session().await else {
        client
            .write_all(&[0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0])
            .await?;
        return Ok(());
    };

    let mut remote = match session.dial(&host, port).await {
        Ok(remote) => remote,
        Err(_) => {
            client
                .write_all(&[0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0])
                .await?;
            return Ok(());
        }
    };

    client
        .write_all(&[0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0])
        .await?;
    inner.record_exit(&host, &session);
    let _ = tokio::io::copy_bidirectional(&mut client, &mut remote).await;
    Ok(())
}
