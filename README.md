# Unroxy

Proxy server with URL rewriting.

## Usage

```bash
# Build and run
go build -o unroxy ./cmd/unroxy && ./unroxy

# Enable rotating upstream proxies
PROXY=1 go build -o unroxy ./cmd/unroxy && PROXY=1 ./unroxy

# Or use Docker
docker run -p 8080:8080 ghcr.io/sachnun/unroxy:latest
```

Access any website through `http://localhost:8080/{domain}/{path}`

**Example:** `http://localhost:8080/example.com`

## Upstream Proxy Mode

Set `PROXY=1` or `PROXY=true` to enable rotating upstream proxies.

- Default is disabled.
- The proxy list is fetched from `https://cdn.jsdelivr.net/gh/proxifly/free-proxy-list@main/proxies/all/data.json`.
- The list is cached in memory for 10 minutes and refreshed lazily on the next request after TTL expires.
- Only `socks5` proxies and `http` proxies with `https: true` are used.
- Requests rotate through the available proxies until one succeeds. If all proxies fail, the request returns `502 Bad Gateway`.
