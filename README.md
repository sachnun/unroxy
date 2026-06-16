# Unroxy

Proxy server with URL rewriting.

## Usage

```bash
docker run \
  -p 8080:8080 \
  ghcr.io/sachnun/unroxy

curl http://localhost:8080/example.com
```

## Development

```bash
go build -o unroxy ./cmd/unroxy && ./unroxy
```
