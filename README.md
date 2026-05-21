# Unroxy

Proxy server with URL rewriting.

## Usage

```bash
docker run \
  -p 8080:8080 \
  ghcr.io/sachnun/unroxy

curl http://localhost:8080/example.com
```

Requests to the target origin always use Webshare rotating SOCKS5 through `p.webshare.io:80`.

Set Webshare credentials with:

```bash
export WEBSHARE_USERNAME=your_username
export WEBSHARE_PASSWORD=your_password
```

Proxy priority is:

1. `socks5`

## Development

```bash
go build -o unroxy ./cmd/unroxy && ./unroxy
```

Access any website through `http://localhost:8080/{domain}/{path}`.
