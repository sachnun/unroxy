package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ProbeTarget is the control endpoint the validator dials through each
// proxy to prove it can relay traffic.
const ProbeTarget = "1.1.1.1:80"

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
	conn, err := p.DialContext(ctx, "tcp", ProbeTarget)
	p.Latency = time.Since(start)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// ProbeProxy verifies a proxy end-to-end against the control target and
// returns the measured latency. A custom ProbeFunc takes precedence (lets
// token-based providers such as Urban avoid burning credentials on every
// probe); otherwise the proxy's own DialContext (or a plain HTTP CONNECT
// for URL-based proxies) is used, and the tunnel is exercised with a real
// HTTP request to prove it actually relays traffic rather than merely
// accepting TCP connections.
func ProbeProxy(ctx context.Context, ps *ProxyState) (time.Duration, bool) {
	if ps == nil {
		return 0, false
	}

	if ps.ProbeFunc != nil {
		lat, err := ps.ProbeFunc(ctx)
		if err != nil {
			return 0, false
		}
		return lat, true
	}

	start := time.Now()
	var conn net.Conn
	var err error
	if ps.DialContext != nil {
		conn, err = ps.DialContext(ctx, "tcp", ProbeTarget)
	} else if ps.URL != nil {
		conn, err = httpProxyConnect(ctx, ps.URL, ProbeTarget)
	} else {
		return 0, false
	}
	if err != nil {
		return 0, false
	}
	defer conn.Close()

	// Exercise the tunnel with a minimal GET through it. Any HTTP status
	// line proves the proxy relays origin traffic.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(HealthTimeout)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return 0, false
	}
	host, _, err := net.SplitHostPort(ProbeTarget)
	if err != nil {
		host = ProbeTarget
	}
	req := fmt.Sprintf("GET /cdn-cgi/trace HTTP/1.1\r\nHost: %s\r\nUser-Agent: unroxy-probe\r\nConnection: close\r\n\r\n", host)
	if _, err := conn.Write([]byte(req)); err != nil {
		return 0, false
	}
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil || n == 0 {
		return 0, false
	}
	if !bytes.Contains(buf[:n], []byte("HTTP/")) {
		return 0, false
	}
	return time.Since(start), true
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
