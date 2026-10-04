use std::sync::Arc;

use futures_util::TryStreamExt;
use futures_util::future::BoxFuture;
use http_body_util::{BodyExt, BodyStream};
use hyper::body::Incoming;
use hyper::{Request, Response};
use unroxy_proxy::{
    Body, Error, ExitCache, Forward, ForwardFactory, ProxyPool, Target, strip_client_headers,
    strip_hop_headers,
};
use unroxy_psiphon::Exit;

use crate::upstream::RotatingTransport;

pub struct WreqFactory;

impl ForwardFactory for WreqFactory {
    fn region_forward(&self, _region: &str, pool: Arc<ProxyPool>) -> Option<Arc<dyn Forward>> {
        Some(Arc::new(WreqForward::new(RotatingTransport::new(pool))))
    }

    fn default_forward(&self, pool: Arc<ProxyPool>) -> Option<Arc<dyn Forward>> {
        Some(Arc::new(WreqForward::new(RotatingTransport::new(pool))))
    }
}

pub struct WreqForward {
    transport: Arc<RotatingTransport>,
}

impl WreqForward {
    pub fn new(transport: Arc<RotatingTransport>) -> Self {
        Self { transport }
    }
}

impl Forward for WreqForward {
    fn forward<'a>(
        &'a self,
        request: Request<Incoming>,
        target: Target,
        exits: &'a ExitCache,
    ) -> BoxFuture<'a, Result<Response<Body>, Error>> {
        Box::pin(async move { send(self, request, target, exits).await })
    }
}

async fn send(
    forward: &WreqForward,
    request: Request<Incoming>,
    target: Target,
    exits: &ExitCache,
) -> Result<Response<Body>, Error> {
    let (parts, body) = request.into_parts();
    let uri = target.uri()?;

    let method = parts.method.clone();
    let mut outgoing = Request::builder().method(parts.method).uri(uri);
    {
        let headers = outgoing.headers_mut().expect("request headers");
        *headers = parts.headers;
        strip_client_headers(headers);
        strip_hop_headers(headers);
        if let Ok(value) = hyper::header::HeaderValue::from_str(&target.host) {
            headers.insert(hyper::header::HOST, value);
        }
    }
    let body = BodyStream::new(body)
        .map_ok(|frame| frame.into_data().unwrap_or_default())
        .map_err(|err| std::io::Error::other(err.to_string()));
    let outgoing = outgoing
        .body(wreq::Body::wrap_stream(body))
        .expect("request builds");

    let (candidate, response) = forward
        .transport
        .request(outgoing)
        .await
        .map_err(|err| Error::Forward(err.to_string()))?;
    let status = response.status();
    exits.record(&target.host, candidate.tunnel.exit_for(&target.host));
    tracing::info!("{method} {} -> {status} ({})", target.host, candidate.key);

    let exit = exits.get(&target.host);
    let response: http::Response<wreq::Body> = response.into();
    let (parts, body) = response.into_parts();
    let body = body
        .map_err(|err| std::io::Error::other(err.to_string()))
        .boxed();

    let mut out = Response::builder()
        .status(status)
        .body(body)
        .expect("response builds");
    *out.headers_mut() = parts.headers;
    strip_hop_headers(out.headers_mut());
    set_egress_headers(out.headers_mut(), exit.as_ref()).await;
    Ok(out)
}

async fn set_egress_headers(headers: &mut hyper::HeaderMap, exit: Option<&Exit>) {
    let Some(exit) = exit else {
        return;
    };
    if exit.ip.is_empty() {
        return;
    }
    if let Ok(value) = exit.ip.parse() {
        headers.insert("x-unroxy-ip", value);
    }
    let isp = crate::geo::lookup_within(&exit.ip, crate::geo::LOOKUP_TIMEOUT)
        .await
        .isp;
    if !isp.is_empty()
        && let Ok(value) = isp.parse()
    {
        headers.insert("x-unroxy-isp", value);
    }
}
