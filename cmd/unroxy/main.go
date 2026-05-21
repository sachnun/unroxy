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
	logger.Printf("Usage: http://localhost:%s/{domain}/{path}", port)

	if err := http.ListenAndServe(":"+port, handler); err != nil {
		logger.Fatalf("Server failed: %v", err)
	}
}

func newUpstreamTransport(logger *log.Logger) http.RoundTripper {
	if logger == nil {
		logger = log.Default()
	}

	pool, err := NewWebshareProxyPool(logger, os.Getenv(webshareUsernameEnv), os.Getenv(websharePasswordEnv))
	if err != nil {
		logger.Printf("Webshare proxy not ready")
		pool = NewProxyPool(logger, nil)
	}

	logger.Printf("Proxy ready: %s:%s", webshareProxyHost, webshareProxyPort)

	return NewRotatingProxyTransport(pool)
}
