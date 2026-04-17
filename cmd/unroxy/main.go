package main

import (
	"context"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sort"
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
	allowedProtocols := parseProxyProtocols(os.Getenv("PROXY"))
	if len(allowedProtocols) == 0 {
		logger.Printf("Upstream proxy mode disabled")
		return nil
	}

	pool := NewProxyPool(logger, allowedProtocols)
	if err := pool.Refresh(context.Background()); err != nil {
		logger.Printf("Initial proxy list refresh failed: %v", err)
	} else {
		logger.Printf("Loaded %d upstream proxies", pool.Count())
	}

	logger.Printf("Upstream proxy mode enabled for protocols: %s", strings.Join(sortedProxyProtocols(allowedProtocols), ","))

	return NewRotatingProxyTransport(pool)
}

func proxyEnabled(value string) bool {
	return len(parseProxyProtocols(value)) > 0
}

func parseProxyProtocols(value string) map[string]struct{} {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}

	allowed := make(map[string]struct{})
	for _, part := range strings.Split(value, ",") {
		switch strings.ToLower(strings.TrimSpace(part)) {
		case "", "0", "false":
			continue
		case "1", "true", "all":
			allowProxyProtocols(allowed, "socks", "socks5", "http", "https")
		case "sock":
			allowProxyProtocols(allowed, "socks", "socks5")
		case "http":
			allowProxyProtocols(allowed, "http", "https")
		}
	}

	if len(allowed) == 0 {
		return nil
	}

	return allowed
}

func allowProxyProtocols(allowed map[string]struct{}, protocols ...string) {
	for _, protocol := range protocols {
		allowed[protocol] = struct{}{}
	}
}

func sortedProxyProtocols(allowed map[string]struct{}) []string {
	protocols := make([]string, 0, len(allowed))
	for protocol := range allowed {
		protocols = append(protocols, protocol)
	}
	sort.Strings(protocols)
	return protocols
}
