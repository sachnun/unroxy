//! A tiny HTTP server for tests that would otherwise need a live service.

use std::io::{Read, Write};
use std::net::{TcpListener, TcpStream};
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;

pub struct Server {
    pub url: String,
    hits: Arc<AtomicUsize>,
}

impl Server {
    pub fn hits(&self) -> usize {
        self.hits.load(Ordering::Relaxed)
    }
}

/// Serves `body` for every request, counting hits.
pub fn serve(body: &str) -> Server {
    serve_slow(body, Duration::ZERO)
}

/// Serves `body` after `delay`, so a client timeout can be exercised.
pub fn serve_slow(body: &str, delay: Duration) -> Server {
    let listener = TcpListener::bind("127.0.0.1:0").expect("bind test server");
    let url = format!("http://{}", listener.local_addr().unwrap());
    let hits = Arc::new(AtomicUsize::new(0));
    let counter = Arc::clone(&hits);
    let body = body.to_string();

    std::thread::spawn(move || {
        for stream in listener.incoming() {
            let Ok(stream) = stream else { continue };
            counter.fetch_add(1, Ordering::Relaxed);
            let body = body.clone();
            std::thread::spawn(move || respond(stream, &body, delay));
        }
    });

    Server { url, hits }
}

fn respond(mut stream: TcpStream, body: &str, delay: Duration) {
    let mut buffer = [0u8; 2048];
    let _ = stream.read(&mut buffer);
    if !delay.is_zero() {
        std::thread::sleep(delay);
    }
    let response = format!(
        "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.len()
    );
    let _ = stream.write_all(response.as_bytes());
}
