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

	logger := log.Default()
	transport, err := newUpstreamTransport(logger)
	if err != nil {
		logger.Fatalf("Upstream proxy initialization failed: %v", err)
	}

	handler := NewProxyHandlerWithLoggerAndTransport(logger, transport)

	logger.Printf("Unroxy running on :%s", port)
	logger.Printf("Usage: http://localhost:%s/{domain}/{path}", port)

	if err := http.ListenAndServe(":"+port, handler); err != nil {
		logger.Fatalf("Server failed: %v", err)
	}
}

func newUpstreamTransport(logger *log.Logger) (http.RoundTripper, error) {
	allowedProtocols, invalidValues := parseProxyProtocolConfig(os.Getenv("PROXY"))
	return buildUpstreamTransport(logger, allowedProtocols, invalidValues, nil)
}

func buildUpstreamTransport(logger *log.Logger, allowedProtocols map[string]struct{}, invalidValues []string, pool *ProxyPool) (http.RoundTripper, error) {
	if logger == nil {
		logger = log.Default()
	}

	if len(invalidValues) > 0 {
		logger.Printf("Ignoring unknown PROXY values: %s", strings.Join(invalidValues, ","))
	}

	if len(allowedProtocols) == 0 {
		logger.Printf("Upstream proxy mode disabled")
		return nil, nil
	}

	if pool == nil {
		pool = NewProxyPool(logger, allowedProtocols)
	}

	if err := pool.Refresh(context.Background()); err != nil {
		return nil, err
	}

	logger.Printf("Loaded %d upstream proxies", pool.Count())

	activeCount, inactiveCount, err := pool.PruneInactive(context.Background())
	if err != nil {
		return nil, err
	}

	logger.Printf("Initial proxy health check complete: %d active, %d inactive", activeCount, inactiveCount)
	if activeCount == 0 {
		return nil, errNoUpstreamProxy
	}

	logger.Printf("Upstream proxy mode enabled for protocols: %s", strings.Join(sortedProxyProtocols(allowedProtocols), ","))

	return NewRotatingProxyTransport(pool), nil
}

func proxyEnabled(value string) bool {
	return len(parseProxyProtocols(value)) > 0
}

func parseProxyProtocols(value string) map[string]struct{} {
	allowed, _ := parseProxyProtocolConfig(value)
	return allowed
}

func parseProxyProtocolConfig(value string) (map[string]struct{}, []string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}

	allowed := make(map[string]struct{})
	hasNone := false
	seenInvalid := make(map[string]struct{})
	invalid := make([]string, 0)
	for _, part := range strings.Split(value, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		switch part {
		case "", "none":
			hasNone = true
		case "all":
			allowProxyProtocols(allowed, "socks", "socks5", "http", "https")
		case "sock":
			allowProxyProtocols(allowed, "socks", "socks5")
		case "http":
			allowProxyProtocols(allowed, "http", "https")
		default:
			if _, ok := seenInvalid[part]; !ok {
				seenInvalid[part] = struct{}{}
				invalid = append(invalid, part)
			}
		}
	}

	if hasNone {
		return nil, invalid
	}

	if len(allowed) == 0 {
		return nil, invalid
	}

	return allowed, invalid
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
