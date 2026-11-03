use std::sync::Arc;

use crate::proxy::{
    Body, Error, ExitCache, Forward, ForwardFactory, ProxyPool, Target, empty_body,
    split_authority, strip_client_headers, strip_hop_headers,
};
use futures_util::future::BoxFuture;
use futures_util::stream;
use http_body_util::{BodyExt, StreamBody};
use hyper::body::{Frame, Incoming};
use hyper::{Method, Request, Response, StatusCode};
use tokio::io::{AsyncReadExt, AsyncWriteExt};

const CHUNK: usize = 16 * 1024;
const MAX_HEAD: usize = 256 * 1024;

pub struct DialFactory;

impl ForwardFactory for DialFactory {
    fn region_forward(&self, _region: &str, pool: Arc<ProxyPool>) -> Option<Arc<dyn Forward>> {
        Some(Arc::new(DialForward { pool }))
    }

    fn default_forward(&self, pool: Arc<ProxyPool>) -> Option<Arc<dyn Forward>> {
        Some(Arc::new(DialForward { pool }))
    }
}

pub struct DialForward {
    pool: Arc<ProxyPool>,
}

impl Forward for DialForward {
    fn forward<'a>(
        &'a self,
        request: Request<Incoming>,
        target: Target,
        _exits: &'a ExitCache,
    ) -> BoxFuture<'a, Result<Response<Body>, Error>> {
        Box::pin(async move { send(self, request, target).await })
    }
}

async fn send(
    forward: &DialForward,
    request: Request<Incoming>,
    target: Target,
) -> Result<Response<Body>, Error> {
    let (host, port) = split_authority(&target.host, &target.scheme);
    let Some(candidate) = forward.pool.pick(&host).await else {
        return Err(Error::UnknownRegion);
    };
    let mut stream = candidate
        .tunnel
        .dial(&host, port)
        .await
        .map_err(Error::Upstream)?;

    let (parts, body) = request.into_parts();
    let mut headers = parts.headers;
    strip_client_headers(&mut headers);
    strip_hop_headers(&mut headers);
    if let Ok(value) = hyper::header::HeaderValue::from_str(&target.host) {
        headers.insert(hyper::header::HOST, value);
    }
    headers.insert(hyper::header::CONNECTION, "close".parse().expect("static"));

    let mut head = Vec::with_capacity(256);
    head.extend_from_slice(parts.method.as_str().as_bytes());
    head.push(b' ');
    head.extend_from_slice(target.path.as_bytes());
    if !target.query.is_empty() {
        head.push(b'?');
        head.extend_from_slice(target.query.as_bytes());
    }
    head.extend_from_slice(b" HTTP/1.1\r\n");
    for (name, value) in headers.iter() {
        head.extend_from_slice(name.as_str().as_bytes());
        head.extend_from_slice(b": ");
        head.extend_from_slice(value.as_bytes());
        head.extend_from_slice(b"\r\n");
    }
    head.extend_from_slice(b"\r\n");
    stream
        .write_all(&head)
        .await
        .map_err(|err| Error::Upstream(err.to_string()))?;

    let method = parts.method;
    let mut body = body;
    while let Some(frame) = body.frame().await {
        let frame = frame.map_err(|err| Error::Upstream(err.to_string()))?;
        if let Some(data) = frame.data_ref() {
            stream
                .write_all(data)
                .await
                .map_err(|err| Error::Upstream(err.to_string()))?;
        }
    }
    stream
        .flush()
        .await
        .map_err(|err| Error::Upstream(err.to_string()))?;

    let mut buffer = Vec::with_capacity(CHUNK);
    let mut chunk = vec![0u8; CHUNK];
    let head_end = loop {
        if let Some(position) = find_head_end(&buffer) {
            break position;
        }
        let read = stream
            .read(&mut chunk)
            .await
            .map_err(|err| Error::Upstream(err.to_string()))?;
        if read == 0 {
            return Err(Error::Upstream("empty response".into()));
        }
        buffer.extend_from_slice(&chunk[..read]);
        if buffer.len() > MAX_HEAD {
            return Err(Error::Upstream("response head too large".into()));
        }
    };

    let (status, mut response_headers) = parse_head(&buffer[..head_end])?;
    strip_hop_headers(&mut response_headers);

    let rest = buffer[head_end..].to_vec();
    let body = if method == Method::HEAD {
        empty_body()
    } else {
        streaming(stream, rest)
    };

    let mut out = Response::builder()
        .status(status)
        .body(body)
        .expect("response builds");
    *out.headers_mut() = response_headers;
    Ok(out)
}

fn streaming(stream: Box<dyn crate::proxy::Connection>, rest: Vec<u8>) -> Body {
    let (sender, receiver) = tokio::sync::mpsc::channel::<Vec<u8>>(16);
    tokio::spawn(async move {
        let mut stream = stream;
        if !rest.is_empty() && sender.send(rest).await.is_err() {
            return;
        }
        let mut chunk = vec![0u8; CHUNK];
        loop {
            match stream.read(&mut chunk).await {
                Ok(0) => break,
                Ok(read) => {
                    if sender.send(chunk[..read].to_vec()).await.is_err() {
                        break;
                    }
                }
                Err(_) => break,
            }
        }
    });
    let frames = stream::unfold(receiver, |mut receiver| async move {
        let chunk = receiver.recv().await?;
        Some((Ok(Frame::data(bytes::Bytes::from(chunk))), receiver))
    });
    StreamBody::new(frames).boxed()
}

fn parse_head(head: &[u8]) -> Result<(StatusCode, hyper::HeaderMap), Error> {
    let mut lines = split_lines(head);
    let Some(status_line) = lines.next() else {
        return Err(Error::Upstream("malformed status line".into()));
    };
    let status = status_line
        .split_whitespace()
        .nth(1)
        .and_then(|code| code.parse::<u16>().ok())
        .and_then(|code| StatusCode::from_u16(code).ok())
        .ok_or_else(|| Error::Upstream("malformed status".into()))?;
    let mut headers = hyper::HeaderMap::new();
    for line in lines {
        let Some((name, value)) = line.split_once(':') else {
            continue;
        };
        if let (Ok(name), Ok(value)) = (
            hyper::header::HeaderName::from_bytes(name.trim().as_bytes()),
            hyper::header::HeaderValue::from_str(value.trim()),
        ) {
            headers.append(name, value);
        }
    }
    Ok((status, headers))
}

fn find_head_end(buffer: &[u8]) -> Option<usize> {
    buffer
        .windows(4)
        .position(|window| window == b"\r\n\r\n")
        .map(|position| position + 4)
}

fn split_lines(head: &[u8]) -> impl Iterator<Item = &str> {
    std::str::from_utf8(head)
        .unwrap_or_default()
        .lines()
        .map(str::trim_end)
        .filter(|line| !line.is_empty())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_status_and_headers() {
        let head = b"HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nX-A: 1\r\nX-A: 2\r\n\r\n";
        let (status, headers) = parse_head(head).expect("head");
        assert_eq!(status, StatusCode::OK);
        assert_eq!(headers.get("content-type").expect("type"), "text/plain");
        assert_eq!(headers.get_all("x-a").iter().count(), 2);
    }

    #[test]
    fn parses_error_status() {
        let (status, _) = parse_head(b"HTTP/1.1 502 Bad Gateway\r\n\r\n").expect("head");
        assert_eq!(status, StatusCode::BAD_GATEWAY);
    }

    #[test]
    fn rejects_malformed_status() {
        assert!(parse_head(b"HTTP/1.1\r\n\r\n").is_err());
        assert!(parse_head(b"garbage\r\n\r\n").is_err());
        assert!(parse_head(b"").is_err());
    }

    #[test]
    fn skips_invalid_header_lines() {
        let head = b"HTTP/1.1 200 OK\r\nnot-a-header\r\nX-B: ok\r\n\r\n";
        let (_, headers) = parse_head(head).expect("head");
        assert_eq!(headers.get("x-b").expect("header"), "ok");
    }

    #[test]
    fn finds_end_of_head() {
        assert_eq!(find_head_end(b"HTTP/1.1 200\r\n\r\nbody"), Some(16));
        assert_eq!(find_head_end(b"HTTP/1.1 200\r\n"), None);
    }

    #[test]
    fn splits_lines_without_empty_entries() {
        let lines: Vec<&str> = split_lines(b"HTTP/1.1 200\r\n\r\n").collect();
        assert_eq!(lines, vec!["HTTP/1.1 200"]);
    }
}
