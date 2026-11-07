use std::convert::Infallible;
use std::net::SocketAddr;
use std::sync::{Arc, RwLock};

use bytes::Bytes;
use futures_util::future::BoxFuture;
use http_body_util::{BodyExt, Full, combinators::BoxBody};
use hyper::body::Incoming;
use hyper::{Request, Response, StatusCode, Uri};
use hyper_util::rt::TokioIo;
use tokio::net::TcpListener;

use crate::proxy::exit::ExitCache;
use crate::proxy::pool::{Protocol, ProxyPool};

pub type Body = BoxBody<Bytes, std::io::Error>;

pub fn empty_body() -> Body {
    Full::new(Bytes::new())
        .map_err(|never| match never {})
        .boxed()
}

pub fn full_body(bytes: impl Into<Bytes>) -> Body {
    Full::new(bytes.into())
        .map_err(|never| match never {})
        .boxed()
}

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("unknown proxy region")]
    UnknownRegion,
    #[error("unsupported scheme")]
    UnsupportedScheme,
    #[error("missing target host")]
    MissingTarget,
    #[error("forwarding unavailable")]
    ForwardUnavailable,
    #[error("ssh: {0}")]
    Tunnel(#[from] crate::ssh::Error),
    #[error("upstream: {0}")]
    Upstream(String),
    #[error("io: {0}")]
    Io(#[from] std::io::Error),
    #[error("forward: {0}")]
    Forward(String),
}

#[derive(Clone)]
pub struct Target {
    pub scheme: String,
    pub host: String,
    pub path: String,
    pub query: String,
}

impl Target {
    pub fn uri(&self) -> Result<Uri, Error> {
        let mut uri = format!("{}://{}{}", self.scheme, self.host, self.path);
        if !self.query.is_empty() {
            uri.push('?');
            uri.push_str(&self.query);
        }
        uri.parse().map_err(|_| Error::MissingTarget)
    }
}

pub trait Forward: Send + Sync + 'static {
    fn forward<'a>(
        &'a self,
        request: Request<Incoming>,
        target: Target,
        exits: &'a ExitCache,
    ) -> BoxFuture<'a, Result<Response<Body>, Error>>;
}

pub struct Variant {
    pub protocol: Protocol,
    pub pool: Arc<ProxyPool>,
    pub forward: Option<Arc<dyn Forward>>,
}

pub struct Region {
    pub name: String,
    pub username: String,
    variants: RwLock<Vec<Variant>>,
    merged: Arc<ProxyPool>,
}

impl Region {
    pub fn new(name: String, username: String) -> Self {
        Self {
            name,
            username,
            variants: RwLock::new(Vec::new()),
            merged: ProxyPool::new(Vec::new()),
        }
    }

    pub fn add(&self, protocol: Protocol, pool: Arc<ProxyPool>, forward: Option<Arc<dyn Forward>>) {
        let mut variants = self.variants.write().expect("variant lock");
        match variants
            .iter_mut()
            .find(|variant| variant.protocol == protocol)
        {
            Some(existing) => {
                if existing.forward.is_none() {
                    existing.forward = forward;
                }
            }
            None => variants.push(Variant {
                protocol,
                pool: Arc::clone(&pool),
                forward,
            }),
        }
        drop(variants);
        self.merged.add_child(pool);
    }

    pub fn pool(&self, protocol: Option<Protocol>) -> Arc<ProxyPool> {
        match protocol {
            Some(wanted) => self
                .variants
                .read()
                .expect("variant lock")
                .iter()
                .find(|variant| variant.protocol == wanted)
                .map(|variant| Arc::clone(&variant.pool))
                .unwrap_or_else(|| ProxyPool::new(Vec::new())),
            None => Arc::clone(&self.merged),
        }
    }

    pub fn forward(&self, protocol: Option<Protocol>) -> Option<Arc<dyn Forward>> {
        let variants = self.variants.read().expect("variant lock");
        match protocol {
            Some(wanted) => variants
                .iter()
                .find(|variant| variant.protocol == wanted)
                .and_then(|variant| variant.forward.clone()),
            None => variants
                .iter()
                .find(|variant| variant.protocol == Protocol::Ssh)
                .or_else(|| variants.first())
                .and_then(|variant| variant.forward.clone()),
        }
    }

    pub fn protocols(&self) -> Vec<Protocol> {
        self.variants
            .read()
            .expect("variant lock")
            .iter()
            .map(|variant| variant.protocol)
            .collect()
    }
}

pub struct PoolStat {
    pub name: String,
    pub protocols: Vec<Protocol>,
    pub proxies: usize,
    pub tunnels: usize,
    pub usable: usize,
}

pub struct Proxy {
    default_pool: Arc<ProxyPool>,
    default_forward: RwLock<Option<Arc<dyn Forward>>>,
    protocol_pools: RwLock<Vec<(Protocol, Arc<ProxyPool>)>>,
    protocol_forwards: RwLock<Vec<(Protocol, Arc<dyn Forward>)>>,
    regions: RwLock<Vec<Arc<Region>>>,
    pub exits: ExitCache,
}

impl Proxy {
    pub fn new(
        default_pool: Arc<ProxyPool>,
        default_forward: Option<Arc<dyn Forward>>,
    ) -> Arc<Self> {
        Arc::new(Self {
            default_pool,
            default_forward: RwLock::new(default_forward),
            protocol_pools: RwLock::new(Vec::new()),
            protocol_forwards: RwLock::new(Vec::new()),
            regions: RwLock::new(Vec::new()),
            exits: ExitCache::default(),
        })
    }

    pub fn set_protocol_route(
        &self,
        protocol: Protocol,
        pool: Arc<ProxyPool>,
        forward: Arc<dyn Forward>,
    ) {
        let mut pools = self.protocol_pools.write().expect("protocol pool lock");
        pools.retain(|(known, _)| *known != protocol);
        pools.push((protocol, pool));
        drop(pools);

        let mut forwards = self.protocol_forwards.write().expect("protocol lock");
        forwards.retain(|(known, _)| *known != protocol);
        forwards.push((protocol, forward));
    }

    pub fn add_region(&self, region: Arc<Region>) {
        let mut regions = self.regions.write().expect("region lock");
        let Some(existing) = regions
            .iter()
            .find(|current| current.username.eq_ignore_ascii_case(&region.username))
            .cloned()
        else {
            regions.push(region);
            return;
        };
        for protocol in region.protocols() {
            existing.add(
                protocol,
                region.pool(Some(protocol)),
                region.forward(Some(protocol)),
            );
        }
    }

    pub fn region(&self, username: &str) -> Option<Arc<Region>> {
        if username.is_empty() {
            return None;
        }
        self.regions
            .read()
            .expect("region lock")
            .iter()
            .find(|region| region.username.eq_ignore_ascii_case(username))
            .cloned()
    }

    fn forward_for(&self, username: &str, protocol: Option<Protocol>) -> Option<Arc<dyn Forward>> {
        if let Some(region) = self.region(username)
            && let Some(forward) = region.forward(protocol)
        {
            return Some(forward);
        }
        if let Some(protocol) = protocol {
            return self
                .protocol_forwards
                .read()
                .expect("protocol lock")
                .iter()
                .find(|(known, _)| *known == protocol)
                .map(|(_, forward)| Arc::clone(forward));
        }
        self.default_forward.read().expect("forward lock").clone()
    }

    fn pool_for(&self, username: &str, protocol: Option<Protocol>) -> Arc<ProxyPool> {
        if let Some(region) = self.region(username) {
            return region.pool(protocol);
        }
        if let Some(protocol) = protocol {
            let pools = self.protocol_pools.read().expect("protocol pool lock");
            if let Some((_, pool)) = pools.iter().find(|(known, _)| *known == protocol) {
                return Arc::clone(pool);
            }
            return ProxyPool::new(Vec::new());
        }
        Arc::clone(&self.default_pool)
    }

    pub fn stats(&self) -> Vec<PoolStat> {
        self.regions
            .read()
            .expect("region lock")
            .iter()
            .map(|region| {
                let pool = region.pool(None);
                PoolStat {
                    name: region.name.clone(),
                    protocols: region.protocols(),
                    proxies: pool.count(),
                    tunnels: pool.tunnel_count(),
                    usable: pool.usable_count(),
                }
            })
            .collect()
    }
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Route {
    pub region: Option<String>,
    pub protocol: Option<Protocol>,
}

impl Route {
    pub fn region(&self) -> &str {
        self.region.as_deref().unwrap_or_default()
    }
}

pub struct Server {
    proxy: Arc<Proxy>,
}

impl Server {
    pub fn new(proxy: Arc<Proxy>) -> Self {
        Self { proxy }
    }

    pub async fn serve(self: Arc<Self>, listener: TcpListener) -> std::io::Result<()> {
        loop {
            let (stream, peer) = listener.accept().await?;
            let server = Arc::clone(&self);
            tokio::spawn(async move {
                if let Err(err) = server.serve_connection(stream, peer).await {
                    tracing::debug!("connection from {peer} closed: {err}");
                }
            });
        }
    }

    async fn serve_connection(
        &self,
        stream: tokio::net::TcpStream,
        peer: SocketAddr,
    ) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
        let io = TokioIo::new(stream);
        let proxy = Arc::clone(&self.proxy);
        let service = hyper::service::service_fn(move |request| {
            let proxy = Arc::clone(&proxy);
            async move { handle(proxy, request, peer).await }
        });
        hyper::server::conn::http1::Builder::new()
            .preserve_header_case(true)
            .serve_connection(io, service)
            .with_upgrades()
            .await?;
        Ok(())
    }
}

async fn handle(
    proxy: Arc<Proxy>,
    request: Request<Incoming>,
    _peer: SocketAddr,
) -> Result<Response<Body>, Infallible> {
    if request.method() == hyper::Method::CONNECT {
        return Ok(handle_connect(proxy, request).await);
    }

    let response = if request.uri().host().is_some() {
        absolute(proxy, request).await
    } else {
        rewrite(proxy, request).await
    };
    Ok(response.unwrap_or_else(|err| error_response(&err)))
}

fn error_response(err: &Error) -> Response<Body> {
    let status = match err {
        Error::UnsupportedScheme | Error::MissingTarget => StatusCode::BAD_REQUEST,
        Error::ForwardUnavailable => StatusCode::NOT_IMPLEMENTED,
        Error::UnknownRegion | Error::Tunnel(_) | Error::Forward(_) | Error::Upstream(_) => {
            StatusCode::BAD_GATEWAY
        }
        Error::Io(_) => StatusCode::SERVICE_UNAVAILABLE,
    };
    Response::builder()
        .status(status)
        .body(full_body(err.to_string()))
        .expect("static response")
}

async fn handle_connect(proxy: Arc<Proxy>, request: Request<Incoming>) -> Response<Body> {
    let Some(authority) = request
        .uri()
        .authority()
        .map(|authority| authority.to_string())
        .or_else(|| {
            request
                .headers()
                .get(hyper::header::HOST)
                .and_then(|v| v.to_str().ok())
                .map(str::to_string)
        })
    else {
        return error_response(&Error::MissingTarget);
    };
    let (host, port) = split_authority(&authority, "https");

    let username = auth_username(request.headers());
    let pool = proxy.pool_for(&username, None);
    let Some(candidate) = pool.pick(&host).await else {
        return error_response(&Error::UnknownRegion);
    };
    let upstream = match candidate.tunnel.dial(&host, port).await {
        Ok(stream) => stream,
        Err(err) => {
            tracing::warn!("CONNECT {host}:{port}: {err}");
            return error_response(&Error::Upstream(err));
        }
    };
    proxy.exits.record(&host, candidate.tunnel.exit_for(&host));
    tracing::info!(
        "CONNECT tunnel {host}:{port} established via {} {}",
        candidate.protocol.as_str(),
        candidate.key
    );

    let on_upgrade = hyper::upgrade::on(request);
    tokio::spawn(async move {
        let Ok(upgraded) = on_upgrade.await else {
            return;
        };
        let mut client = TokioIo::new(upgraded);
        let mut upstream = upstream;
        if let Err(err) = tokio::io::copy_bidirectional(&mut client, &mut upstream).await {
            tracing::debug!("CONNECT {host}:{port} closed: {err}");
        }
    });

    Response::builder()
        .status(StatusCode::OK)
        .body(empty_body())
        .expect("static response")
}

async fn absolute(proxy: Arc<Proxy>, request: Request<Incoming>) -> Result<Response<Body>, Error> {
    let uri = request.uri().clone();
    let scheme = uri.scheme_str().unwrap_or("http").to_string();
    if scheme != "http" && scheme != "https" {
        return Err(Error::UnsupportedScheme);
    }

    let username = auth_username(request.headers());
    let forward = proxy
        .forward_for(&username, None)
        .ok_or(Error::ForwardUnavailable)?;
    let target = Target {
        scheme,
        host: uri
            .authority()
            .map(|authority| authority.to_string())
            .unwrap_or_default(),
        path: uri.path().to_string(),
        query: uri.query().unwrap_or_default().to_string(),
    };
    forward.forward(request, target, &proxy.exits).await
}

async fn rewrite(proxy: Arc<Proxy>, request: Request<Incoming>) -> Result<Response<Body>, Error> {
    let Some((route, target)) = parse_path(request.uri()) else {
        return Ok(index_page(
            &proxy,
            request.headers().get(hyper::header::HOST),
        ));
    };

    let forward = proxy
        .forward_for(route.region(), route.protocol)
        .ok_or(Error::ForwardUnavailable)?;
    forward.forward(request, target, &proxy.exits).await
}

pub fn strip_client_headers(headers: &mut hyper::HeaderMap) {
    for name in [
        "x-forwarded-for",
        "x-real-ip",
        "x-originating-ip",
        "true-client-ip",
        "client-ip",
        "forwarded",
        "x-forwarded-host",
        "x-forwarded-proto",
        "cf-connecting-ip",
        "proxy-authorization",
    ] {
        headers.remove(name);
    }
}

pub fn strip_hop_headers(headers: &mut hyper::HeaderMap) {
    for name in [
        "connection",
        "proxy-connection",
        "keep-alive",
        "proxy-authenticate",
        "proxy-authorization",
        "te",
        "trailer",
        "transfer-encoding",
        "upgrade",
    ] {
        headers.remove(name);
    }
}

pub fn auth_username(headers: &hyper::HeaderMap) -> String {
    let header = headers
        .get(hyper::header::PROXY_AUTHORIZATION)
        .or_else(|| headers.get(hyper::header::AUTHORIZATION));
    let Some(header) = header.and_then(|value| value.to_str().ok()) else {
        return String::new();
    };
    let Some(encoded) = header.strip_prefix("Basic ") else {
        return String::new();
    };
    decode_username(encoded.trim())
}

fn decode_username(encoded: &str) -> String {
    use base64::Engine;
    let Ok(decoded) = base64::engine::general_purpose::STANDARD.decode(encoded) else {
        return String::new();
    };
    let decoded = String::from_utf8_lossy(&decoded);
    decoded.split(':').next().unwrap_or_default().to_string()
}

pub fn split_authority(authority: &str, scheme: &str) -> (String, u16) {
    if let Some((host, port)) = authority.rsplit_once(':')
        && let Ok(port) = port.parse::<u16>()
        && (!host.contains(']') || host.ends_with(']'))
    {
        return (host.trim_matches(['[', ']']).to_string(), port);
    }
    let port = if scheme == "https" { 443 } else { 80 };
    (authority.trim_matches(['[', ']']).to_string(), port)
}

fn parse_path(uri: &Uri) -> Option<(Route, Target)> {
    let mut rest = uri.path().trim_start_matches('/');
    if rest.is_empty() {
        return None;
    }

    let mut protocol = None;
    let mut region = None;
    for _ in 0..2 {
        let Some((head, tail)) = rest.split_once('/') else {
            break;
        };
        if head.is_empty() || head.contains('.') || head.contains(':') {
            break;
        }
        if protocol.is_none()
            && let Some(parsed) = Protocol::parse(head)
        {
            protocol = Some(parsed);
            rest = tail;
            continue;
        }
        if region.is_none() {
            region = Some(head.to_ascii_uppercase());
            rest = tail;
            continue;
        }
        break;
    }

    let lower = rest.to_ascii_lowercase();
    let (scheme, rest) = if lower.starts_with("https://") {
        ("https", &rest["https://".len()..])
    } else if lower.starts_with("http://") {
        ("http", &rest["http://".len()..])
    } else if lower.starts_with("https:") {
        ("https", &rest["https:".len()..])
    } else if lower.starts_with("http:") {
        ("http", &rest["http:".len()..])
    } else {
        ("https", rest)
    };

    let rest = rest.trim_start_matches('/');
    if rest.is_empty() {
        return None;
    }

    let (domain, path) = match rest.find('/') {
        Some(index) => (&rest[..index], &rest[index..]),
        None => (rest, "/"),
    };
    let domain = domain.rsplit('@').next().unwrap_or(domain);
    if !is_valid_domain(domain) {
        return None;
    }

    Some((
        Route { region, protocol },
        Target {
            scheme: scheme.to_string(),
            host: domain.to_string(),
            path: path.to_string(),
            query: uri.query().unwrap_or_default().to_string(),
        },
    ))
}

fn is_valid_domain(domain: &str) -> bool {
    if domain.is_empty() || domain.len() > 253 {
        return false;
    }
    if domain.parse::<std::net::IpAddr>().is_ok() {
        return true;
    }
    if !domain.contains('.') {
        return false;
    }
    domain.split('.').all(|part| {
        !part.is_empty()
            && part.len() <= 63
            && !part.starts_with('-')
            && !part.ends_with('-')
            && part.bytes().all(|c| c.is_ascii_alphanumeric() || c == b'-')
    })
}

fn index_page(proxy: &Proxy, host: Option<&hyper::header::HeaderValue>) -> Response<Body> {
    let host = host
        .and_then(|value| value.to_str().ok())
        .filter(|value| !value.is_empty())
        .unwrap_or("localhost:8080");

    let mut out = String::new();
    out.push_str("Usage\n─────\n");
    out.push_str(&format!(
        "  HTTP      curl -x http://{host} http://ipwho.is\n"
    ));
    out.push_str(&format!(
        "  CONNECT   curl -x http://{host} https://ipwho.is\n"
    ));
    out.push_str(&format!(
        "  Region    curl -x http://us@{host} https://ipwho.is\n"
    ));
    out.push_str(&format!("  Rewrite   curl http://{host}/ipwho.is\n"));
    out.push_str(&format!(
        "            curl http://{host}/https://ipwho.is\n"
    ));
    out.push_str(&format!("  Protocol  curl http://{host}/ssh/ca/ipwho.is\n"));
    out.push_str(&format!("            curl http://{host}/vpn/ca/ipwho.is\n"));

    let stats = proxy.stats();
    if !stats.is_empty() {
        out.push_str("\nPools\n─────\n");
        for (index, stat) in stats.iter().enumerate() {
            let entry = if stat.tunnels > 0 {
                format!("{}({}/{})", stat.name, stat.usable, stat.tunnels)
            } else {
                format!("{}({})", stat.name, stat.proxies)
            };
            if index % 5 == 0 {
                out.push_str(&format!("  {entry:<12}"));
            } else {
                out.push_str(&format!("{entry:<12}"));
            };
            if index % 5 == 4 || index == stats.len() - 1 {
                out.push('\n');
            }
        }
        let tunnels: usize = stats.iter().map(|stat| stat.tunnels).sum();
        let usable: usize = stats.iter().map(|stat| stat.usable).sum();
        let proxies: usize = stats.iter().map(|stat| stat.proxies).sum();
        let ssh = stats
            .iter()
            .filter(|stat| stat.protocols.contains(&Protocol::Ssh))
            .count();
        let vpn = stats
            .iter()
            .filter(|stat| stat.protocols.contains(&Protocol::Vpn))
            .count();
        if tunnels > 0 {
            out.push_str(&format!("\nTotal: {usable}/{tunnels} usable\n"));
        } else {
            out.push_str(&format!("\nTotal: {proxies} proxies\n"));
        }
        out.push_str(&format!("Pools: {ssh} ssh, {vpn} vpn\n"));
    }

    Response::builder()
        .status(StatusCode::OK)
        .header(hyper::header::CONTENT_TYPE, "text/plain; charset=utf-8")
        .body(full_body(out))
        .expect("static response")
}

#[cfg(test)]
mod tests {
    use super::*;

    use base64::Engine;

    fn headers(pairs: &[(&str, &str)]) -> hyper::HeaderMap {
        let mut map = hyper::HeaderMap::new();
        for (name, value) in pairs {
            map.insert(
                hyper::header::HeaderName::from_bytes(name.as_bytes()).expect("header name"),
                value.parse().expect("header value"),
            );
        }
        map
    }

    #[test]
    fn splits_host_and_port() {
        assert_eq!(
            split_authority("example.com:8443", "https"),
            ("example.com".to_string(), 8443)
        );
        assert_eq!(
            split_authority("example.com", "https"),
            ("example.com".to_string(), 443)
        );
        assert_eq!(
            split_authority("example.com", "http"),
            ("example.com".to_string(), 80)
        );
        assert_eq!(
            split_authority("[::1]:1194", "https"),
            ("::1".to_string(), 1194)
        );
    }

    #[test]
    fn reads_region_from_proxy_authorization() {
        let header = format!(
            "Basic {}",
            base64::engine::general_purpose::STANDARD.encode("sg:secret")
        );
        assert_eq!(
            auth_username(&headers(&[("proxy-authorization", &header)])),
            "sg"
        );
    }

    #[test]
    fn falls_back_to_authorization_header() {
        let header = format!(
            "Basic {}",
            base64::engine::general_purpose::STANDARD.encode("de-VPN")
        );
        assert_eq!(
            auth_username(&headers(&[("authorization", &header)])),
            "de-VPN"
        );
    }

    #[test]
    fn ignores_malformed_credentials() {
        assert_eq!(auth_username(&headers(&[])), "");
        assert_eq!(
            auth_username(&headers(&[("proxy-authorization", "Bearer abc")])),
            ""
        );
        assert_eq!(
            auth_username(&headers(&[("proxy-authorization", "Basic !!!")])),
            ""
        );
    }

    #[test]
    fn parses_region_prefixed_path() {
        let uri: Uri = "/SG/example.com/path?q=1".parse().expect("uri");
        let (route, target) = parse_path(&uri).expect("target");
        assert_eq!(route.region.as_deref(), Some("SG"));
        assert_eq!(route.protocol, None);
        assert_eq!(target.host, "example.com");
        assert_eq!(target.scheme, "https");
        assert_eq!(target.path, "/path");
        assert_eq!(target.query, "q=1");
    }

    #[test]
    fn parses_protocol_prefixed_paths() {
        let uri: Uri = "/ssh/ca/example.com/a".parse().expect("uri");
        let (route, target) = parse_path(&uri).expect("target");
        assert_eq!(route.protocol, Some(Protocol::Ssh));
        assert_eq!(route.region.as_deref(), Some("CA"));
        assert_eq!(target.host, "example.com");
        assert_eq!(target.path, "/a");

        let uri: Uri = "/vpn/de/example.com".parse().expect("uri");
        let (route, target) = parse_path(&uri).expect("target");
        assert_eq!(route.protocol, Some(Protocol::Vpn));
        assert_eq!(route.region.as_deref(), Some("DE"));
        assert_eq!(target.host, "example.com");

        let uri: Uri = "/ssh/example.com".parse().expect("uri");
        let (route, target) = parse_path(&uri).expect("target");
        assert_eq!(route.protocol, Some(Protocol::Ssh));
        assert_eq!(route.region, None);
        assert_eq!(target.host, "example.com");
    }

    #[test]
    fn keeps_region_only_paths_working() {
        let uri: Uri = "/ca/example.com".parse().expect("uri");
        let (route, target) = parse_path(&uri).expect("target");
        assert_eq!(route.protocol, None);
        assert_eq!(route.region.as_deref(), Some("CA"));
        assert_eq!(target.host, "example.com");
    }

    #[test]
    fn parses_path_without_region() {
        let uri: Uri = "/https://example.com/a".parse().expect("uri");
        let (route, target) = parse_path(&uri).expect("target");
        assert_eq!(route.region, None);
        assert_eq!(route.protocol, None);
        assert_eq!(target.scheme, "https");
        assert_eq!(target.host, "example.com");
        assert_eq!(target.path, "/a");
    }

    #[test]
    fn strips_credentials_from_path_host() {
        let uri: Uri = "/user:pass@example.com/".parse().expect("uri");
        let (_, target) = parse_path(&uri).expect("target");
        assert_eq!(target.host, "example.com");
    }

    #[test]
    fn rejects_bad_paths() {
        assert!(parse_path(&"/".parse().expect("uri")).is_none());
        assert!(parse_path(&"/localhost/x".parse().expect("uri")).is_none());
        assert!(parse_path(&"/exa%20mple.com/x".parse().expect("uri")).is_none());
        assert!(parse_path(&"/example..com/x".parse().expect("uri")).is_none());
        assert!(parse_path(&"/ssh/ca/".parse().expect("uri")).is_none());
    }

    #[test]
    fn validates_domains() {
        assert!(is_valid_domain("example.com"));
        assert!(is_valid_domain("1.2.3.4"));
        assert!(!is_valid_domain(""));
        assert!(!is_valid_domain("localhost"));
        assert!(!is_valid_domain("-bad.com"));
        assert!(!is_valid_domain("bad_.com"));
    }

    #[test]
    fn strips_identity_headers() {
        let mut headers = hyper::HeaderMap::new();
        headers.insert("x-forwarded-for", "1.2.3.4".parse().expect("value"));
        headers.insert("proxy-authorization", "Basic x".parse().expect("value"));
        headers.insert("user-agent", "curl".parse().expect("value"));
        strip_client_headers(&mut headers);
        assert!(headers.get("x-forwarded-for").is_none());
        assert!(headers.get("proxy-authorization").is_none());
        assert_eq!(headers.get("user-agent").expect("kept"), "curl");
    }

    #[test]
    fn strips_hop_headers() {
        let mut headers = hyper::HeaderMap::new();
        headers.insert("connection", "keep-alive".parse().expect("value"));
        headers.insert("transfer-encoding", "chunked".parse().expect("value"));
        headers.insert("accept", "*/*".parse().expect("value"));
        strip_hop_headers(&mut headers);
        assert!(headers.get("connection").is_none());
        assert!(headers.get("transfer-encoding").is_none());
        assert_eq!(headers.get("accept").expect("kept"), "*/*");
    }
}
