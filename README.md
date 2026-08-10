## Usage

```bash
docker run \
  -p 8080:8080 \
  ghcr.io/sachnun/unroxy

curl http://localhost:8080/ipwho.is
```

## Development

```bash
go build -o unroxy ./cmd/unroxy && ./unroxy
```
