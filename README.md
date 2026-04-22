# Unroxy

Proxy server with URL rewriting.

## Usage

```bash
docker run \
  -p 8080:8080 \
  ghcr.io/sachnun/unroxy

curl http://localhost:8080/example.com
```

Requests go direct to the target origin by default.

If the target host responds with `403` or `429`, Unroxy marks that host as restricted for a short time and retries through upstream proxies.

Fallback proxy priority is:

1. `socks5`
2. `https`
3. `http`

## Development

```bash
go build -o unroxy ./cmd/unroxy && ./unroxy
```

Access any website through `http://localhost:8080/{domain}/{path}`.
