# Universal Proxy

Proxy server with URL rewriting.

## Usage

```bash
# Build and run
go build && ./universal-proxy

# Or use Docker
docker run -p 8080:8080 ghcr.io/sachnun/universal-proxy:latest
```

Access any website through `http://localhost:8080/{domain}/{path}`

**Example:** `http://localhost:8080/example.com`
