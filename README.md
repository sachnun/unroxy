# Unroxy

Proxy server with URL rewriting.

## Usage

```bash
docker run \
  -p 8080:8080 \
  ghcr.io/sachnun/unroxy

curl http://localhost:8080/example.com
```

Requests to the target origin always use upstream proxies loaded from Geonode.

Proxy priority is:

1. `socks5`
2. `https`
3. `http`

## Development

```bash
go build -o unroxy ./cmd/unroxy && ./unroxy
```

Access any website through `http://localhost:8080/{domain}/{path}`.
