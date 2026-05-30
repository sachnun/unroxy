package main

import (
	"log"
	"net/http"
	"os"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	logger := log.Default()
	handler := NewProxyHandlerWithLoggerAndTransport(logger, newUpstreamTransport(logger))

	logger.Printf("Unroxy running on :%s", port)
	logger.Printf("Rewrite proxy:  http://localhost:%s/{domain}/{path}", port)
	logger.Printf("Forward proxy:  curl -x http://localhost:%s http://example.com", port)
	logger.Printf("CONNECT tunnel: curl -x http://localhost:%s https://example.com", port)

	if err := http.ListenAndServe(":"+port, handler); err != nil {
		logger.Fatalf("Server failed: %v", err)
	}
}

func newUpstreamTransport(logger *log.Logger) http.RoundTripper {
	if logger == nil {
		logger = log.Default()
	}

	pool, err := NewWebshareProxyPool(logger, os.Getenv(webshareAPIKeyEnv))
	if err != nil {
		logger.Printf("Webshare proxy not ready")
		pool = NewProxyPool(logger, nil)
	}

	logger.Printf("Proxy ready: %d Webshare proxies", pool.Count())

	return NewRotatingProxyTransport(pool)
}
