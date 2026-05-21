# Unroxy

Proxy server with URL rewriting.

## Usage

```bash
docker run \
  -e API_KEY=xxx,zzz \
  -p 8080:8080 \
  ghcr.io/sachnun/unroxy

curl http://localhost:8080/example.com
```

Requests to the target origin always use rotating SOCKS5 direct proxies from Webshare free plans. The proxy list refreshes every 6 hours.

## Development

```bash
go build -o unroxy ./cmd/unroxy && ./unroxy
```

Access any website through `http://localhost:8080/{domain}/{path}`.
