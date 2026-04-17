package main

import (
	"context"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strings"
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

	handler := NewProxyHandlerWithTransport(newUpstreamTransport(log.Default()))

	log.Printf("Unroxy running on :%s", port)
	log.Printf("Usage: http://localhost:%s/{domain}/{path}", port)

	if err := http.ListenAndServe(":"+port, handler); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

func newUpstreamTransport(logger *log.Logger) http.RoundTripper {
	if !proxyEnabled(os.Getenv("PROXY")) {
		logger.Printf("Upstream proxy mode disabled")
		return nil
	}

	pool := NewProxyPool(logger)
	if err := pool.Refresh(context.Background()); err != nil {
		logger.Printf("Initial proxy list refresh failed: %v", err)
	} else {
		logger.Printf("Loaded %d upstream proxies", pool.Count())
	}

	logger.Printf("Upstream proxy mode enabled")

	return NewRotatingProxyTransport(pool)
}

func proxyEnabled(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true":
		return true
	default:
		return false
	}
}
