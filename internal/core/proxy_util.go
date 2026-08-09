package core

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DialTimeout bounds a single dial attempt to a proxy or origin.
	DialTimeout = 5 * time.Second
	// HeaderTimeout bounds the wait for response headers through a proxy.
	HeaderTimeout = 20 * time.Second
	// HealthTimeout bounds each connectivity probe during health checks.
	HealthTimeout = 3 * time.Second
	// ProviderFetchTimeout bounds provider list / API fetches.
	ProviderFetchTimeout = 30 * time.Second
	// HealthCheckConcurrency caps parallel health probes.
	HealthCheckConcurrency = 300
	// FailureTTL is how long a failed proxy stays deprioritized per host.
	FailureTTL = 10 * time.Minute
)

// ErrNoUpstreamProxy is returned when a request has no proxy candidates.
var ErrNoUpstreamProxy = errors.New("no upstream proxies available")

// NewProviderHTTPClient returns a client used to fetch proxy provider data.
func NewProviderHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   DialTimeout,
		KeepAlive: 30 * time.Second,
	}
	return &http.Client{
		Timeout:   ProviderFetchTimeout,
		Transport: NewUTLSTransport(dialer.DialContext),
	}
}

// TestProxyReachable probes a proxy by dialing a fixed public endpoint.
func TestProxyReachable(p *ProxyState) bool {
	if p.DialContext == nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), HealthTimeout)
	defer cancel()

	start := time.Now()
	conn, err := p.DialContext(ctx, "tcp", "1.1.1.1:80")
	p.Latency = time.Since(start)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// TestProxiesConcurrently health-checks proxies with bounded concurrency and
// returns only the healthy ones. Progress is logged every 500 probes.
func TestProxiesConcurrently(proxies []*ProxyState, concurrency int, logger *log.Logger) []*ProxyState {
	if len(proxies) == 0 {
		return nil
	}

	sem := make(chan struct{}, concurrency)
	healthy := make([]*ProxyState, 0, len(proxies))
	var mu sync.Mutex
	var wg sync.WaitGroup
	var tested int32
	total := len(proxies)

	for _, p := range proxies {
		sem <- struct{}{}
		wg.Add(1)
		go func(ps *ProxyState) {
			defer wg.Done()
			defer func() { <-sem }()

			if TestProxyReachable(ps) {
				mu.Lock()
				healthy = append(healthy, ps)
				mu.Unlock()
			}

			if n := atomic.AddInt32(&tested, 1); n%500 == 0 || n == int32(total) {
				logger.Printf("[CHECK] %d/%d, %d healthy", n, total, len(healthy))
			}
		}(p)
	}

	wg.Wait()
	logger.Printf("[CHECK] %d proxies, %d healthy", total, len(healthy))
	return healthy
}
