# Unroxy

Universal rotating proxy.

## Usage

```bash
docker run \
  -p 8080:8080 \
  ghcr.io/sachnun/unroxy

curl http://localhost:8080/example.com
```

Requests to the target origin always use rotating SOCKS5 and SOCKS4 public proxies from [Proxifly's free proxy list](https://github.com/proxifly/free-proxy-list). The proxy list is fetched every 15 minutes and each proxy is health-checked via TCP dial before being added to the pool.

## Development

```bash
go build -o unroxy ./cmd/unroxy && ./unroxy
```

Access any website through `http://localhost:8080/{domain}/{path}`.
