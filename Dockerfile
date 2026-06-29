FROM golang:1.26-alpine AS builder
WORKDIR /app
ARG TARGETOS=linux
ARG TARGETARCH=amd64
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/unroxy ./cmd/unroxy

FROM alpine:latest AS usque-fetcher
RUN apk --no-cache add curl unzip
RUN mkdir -p /out && curl -sL "https://github.com/Diniboy1123/usque/releases/download/v4.2.0/usque_4.2.0_linux_amd64.zip" -o /tmp/u.zip \
    && unzip -o /tmp/u.zip -d /tmp \
    && cp /tmp/usque /out/usque

FROM alpine:latest
RUN apk --no-cache add ca-certificates curl
WORKDIR /root/
COPY --from=builder /out/unroxy ./unroxy
COPY --from=usque-fetcher /out/usque ./usque
COPY usque-config.json /tmp/config.json
RUN chmod +x ./usque
EXPOSE 8080
CMD ["./unroxy"]
