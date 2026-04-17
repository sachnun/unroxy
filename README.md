# Unroxy

Proxy server with URL rewriting.

## Usage

```bash
# optional: PROXY=none|http|sock|all
docker run \
  -p 8080:8080 \
  -e PROXY=none \
  ghcr.io/sachnun/unroxy

curl http://localhost:8080/example.com
```

If `PROXY` is enabled, Unroxy blocks startup for an initial proxy health check, keeps only active proxies, and exits when no active proxy is available.

## Development

```bash
go build -o unroxy ./cmd/unroxy && ./unroxy
```

Access any website through `http://localhost:8080/{domain}/{path}`.
