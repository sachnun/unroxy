package main

import (
	"context"
	"log"
	"math/rand"
	"net/http"
	"os"
	"time"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	logger := log.Default()
	handler := NewProxyHandlerWithLoggerAndTransport(logger, newUpstreamTransport(logger))

	logger.Printf("Unroxy running on :%s", port)
	logger.Printf("Usage: http://localhost:%s/{domain}/{path}", port)

	if err := http.ListenAndServe(":"+port, handler); err != nil {
		logger.Fatalf("Server failed: %v", err)
	}
}

func newUpstreamTransport(logger *log.Logger) http.RoundTripper {
	if logger == nil {
		logger = log.Default()
	}

	allowedProtocols := allowedProxyProtocols("socks5", "https", "http")
	pool := NewProxyPool(logger, allowedProtocols)
	if err := pool.Refresh(context.Background()); err != nil {
		logger.Printf("Initial proxy list refresh failed, fallback proxy list unavailable: %v", err)
	} else {
		logger.Printf("Loaded %d fallback upstream proxies", pool.Count())
	}

	logger.Printf("Upstream proxy fallback enabled with priority: socks5,https,http")

	return NewRotatingProxyTransport(pool)
}
