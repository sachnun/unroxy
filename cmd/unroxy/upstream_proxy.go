package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
	"h12.io/socks"
)

const (
	proxyDialTimeout       = 5 * time.Second
	proxyHeaderTimeout     = 20 * time.Second
	proxyHealthTimeout     = 3 * time.Second
	proxiflyFetchTimeout   = 30 * time.Second
	proxiflyRefreshEvery   = 15 * time.Minute
	healthCheckConcurrency = 50
)

var (
	proxiflyBaseURL    = "https://cdn.jsdelivr.net/gh/proxifly/free-proxy-list@main/proxies/"
	errNoUpstreamProxy = errors.New("no upstream proxies available")
)

type proxiflyProxy struct {
	Proxy       string `json:"proxy"`
	Protocol    string `json:"protocol"`
	IP          string `json:"ip"`
	Port        int    `json:"port"`
	HTTPS       bool   `json:"https"`
	Anonymity   string `json:"anonymity"`
	Score       int    `json:"score"`
	GeoLocation struct {
		Country string `json:"country"`
		City    string `json:"city"`
	} `json:"geolocation"`
}

type proxyState struct {
	key           string
	url           *url.URL
	country       string
	healthy       bool
	lastChecked   time.Time
	verifiedAt    time.Time
	verifiedHosts map[string]time.Time
	dialContext   func(ctx context.Context, network, addr string) (net.Conn, error)
}

type proxyCandidate struct {
	key         string
	url         *url.URL
	country     string
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

type ProxyPool struct {
	logger *log.Logger

	mu           sync.RWMutex
	proxies      []*proxyState
	next         int
	nextByHost   map[string]int
	failedByHost map[string]map[string]bool
}

func NewProxyPool(logger *log.Logger, proxies []*proxyState) *ProxyPool {
	if logger == nil {
		logger = log.Default()
	}

	return &ProxyPool{
		logger:  logger,
		proxies: cloneProxyStates(proxies),
	}
}

// ── Proxifly ──────────────────────────────────────────────────────────

func fetchProxiflyProxies() ([]*proxyState, error) {
	client := &http.Client{Timeout: proxiflyFetchTimeout}

	all := make([]*proxyState, 0)

	for _, path := range []string{
		"protocols/socks5/data.json",
		"protocols/socks4/data.json",
	} {
		states, err := fetchProxiflyType(client, path)
		if err != nil {
			continue
		}
		all = append(all, states...)
	}

	if len(all) == 0 {
		return nil, errors.New("no proxifly proxies fetched")
	}

	return all, nil
}

func fetchProxiflyType(client *http.Client, path string) ([]*proxyState, error) {
	url := proxiflyBaseURL + path

	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("proxifly CDN returned status %d", resp.StatusCode)
	}

	var proxies []proxiflyProxy
	if err := json.NewDecoder(resp.Body).Decode(&proxies); err != nil {
		return nil, err
	}

	return proxiflyToProxyStates(proxies), nil
}

func proxiflyToProxyStates(proxies []proxiflyProxy) []*proxyState {
	states := make([]*proxyState, 0, len(proxies))

	for _, p := range proxies {
		scheme := p.Protocol
		if scheme == "" {
			scheme = "socks5"
		}

		rawURL := scheme + "://" + net.JoinHostPort(p.IP, fmt.Sprint(p.Port))

		parsedURL, err := url.Parse(rawURL)
		if err != nil {
			continue
		}

		country := strings.ToUpper(strings.TrimSpace(p.GeoLocation.Country))
		if country == "" {
			country = "XX"
		}

		state := &proxyState{
			key:     rawURL,
			url:     parsedURL,
			country: country,
		}

		if p.Proxy != "" && strings.HasPrefix(p.Proxy, scheme+"://") {
			state.key = p.Proxy
		}

		switch parsedURL.Scheme {
		case "socks5", "socks5h":
			d, err := proxy.FromURL(parsedURL, proxy.Direct)
			if err == nil {
				state.dialContext = d.(proxy.ContextDialer).DialContext
			}
		case "socks4", "socks4a":
			d := socks.Dial(rawURL)
			state.dialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				type dialResult struct {
					conn net.Conn
					err  error
				}
				ch := make(chan dialResult, 1)
				go func() {
					conn, err := d(network, addr)
					ch <- dialResult{conn, err}
				}()
				select {
				case r := <-ch:
					return r.conn, r.err
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		}

		if state.dialContext != nil {
			states = append(states, state)
		}
	}

	return states
}

// NewProxiflyCountryPools fetches proxies from Proxifly, tests them, groups by country.
func NewProxiflyCountryPools(logger *log.Logger) (map[string]*ProxyPool, []*proxyState, error) {
	proxies, err := fetchProxiflyProxies()
	if err != nil {
		return nil, nil, err
	}

	proxies = testProxiesConcurrently(proxies, healthCheckConcurrency, logger)

	groups := make(map[string][]*proxyState)
	for _, p := range proxies {
		code := p.country
		if code == "" {
			code = "XX"
		}
		groups[code] = append(groups[code], p)
	}

	pools := make(map[string]*ProxyPool, len(groups))
	for country, states := range groups {
		pools[country] = NewProxyPool(logger, states)
	}

	return pools, proxies, nil
}

func startProxiflyRefresh(countryPools map[string]*ProxyPool, defaultPool *ProxyPool, logger *log.Logger) {
	go func() {
		ticker := time.NewTicker(proxiflyRefreshEvery)
		defer ticker.Stop()

		for range ticker.C {
			proxies, err := fetchProxiflyProxies()
			if err != nil {
				logger.Printf("Proxifly refresh failed")
				continue
			}

			proxies = testProxiesConcurrently(proxies, healthCheckConcurrency, logger)

			groups := make(map[string][]*proxyState)
			for _, p := range proxies {
				code := p.country
				if code == "" {
					code = "XX"
				}
				groups[code] = append(groups[code], p)
			}

			defaultPool.Replace(proxies)

			for country, states := range groups {
				if pool, ok := countryPools[country]; ok {
					pool.Replace(states)
				}
			}

			logger.Printf("Proxifly refreshed: %d healthy proxies", len(proxies))
		}
	}()
}

// ── Health check ──────────────────────────────────────────────────────

func testProxiesConcurrently(proxies []*proxyState, concurrency int, logger *log.Logger) []*proxyState {
	if len(proxies) == 0 {
		return nil
	}

	sem := make(chan struct{}, concurrency)
	done := make(chan struct{}, len(proxies))
	healthy := make([]*proxyState, 0, len(proxies))
	var mu sync.Mutex

	for _, p := range proxies {
		sem <- struct{}{}
		go func(ps *proxyState) {
			defer func() {
				<-sem
				done <- struct{}{}
			}()

			if testProxyReachable(ps) {
				mu.Lock()
				healthy = append(healthy, ps)
				mu.Unlock()
			}
		}(p)
	}

	// Wait for all checks to complete
	for i := 0; i < len(proxies); i++ {
		<-done
	}

	return healthy
}

func testProxyReachable(p *proxyState) bool {
	if p.dialContext == nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), proxyHealthTimeout)
	defer cancel()

	conn, err := p.dialContext(ctx, "tcp", "1.1.1.1:80")
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// ── Pool rotation ─────────────────────────────────────────────────────

func (p *ProxyPool) Candidates(now time.Time, targetHost string) []proxyCandidate {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.proxies) == 0 {
		return nil
	}

	start := p.next
	rotationKey := strings.ToLower(strings.TrimSpace(targetHost))
	if rotationKey == "" {
		p.next = (start + 1) % len(p.proxies)
	} else {
		if p.nextByHost == nil {
			p.nextByHost = make(map[string]int)
		}
		start = p.nextByHost[rotationKey] % len(p.proxies)
		p.nextByHost[rotationKey] = (start + 1) % len(p.proxies)
	}

	ready := make([]proxyCandidate, 0, len(p.proxies))
	failed := make([]proxyCandidate, 0, len(p.proxies))
	failedKeys := p.failedByHost[rotationKey]

	for i := 0; i < len(p.proxies); i++ {
		state := p.proxies[(start+i)%len(p.proxies)]

		candidate := proxyCandidate{
			key:         state.key,
			url:         cloneURL(state.url),
			country:     state.country,
			dialContext: state.dialContext,
		}
		if failedKeys[state.key] {
			failed = append(failed, candidate)
			continue
		}

		ready = append(ready, candidate)
	}

	ordered := make([]proxyCandidate, 0, len(p.proxies))
	ordered = append(ordered, ready...)
	ordered = append(ordered, failed...)
	return ordered
}

func (p *ProxyPool) MarkSuccess(key, targetHost string) {
	p.markSuccess(key, targetHost, true)
}

func (p *ProxyPool) markSuccess(key, targetHost string, verified bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, state := range p.proxies {
		if state.key != key {
			continue
		}

		now := time.Now()
		state.healthy = true
		state.lastChecked = now
		if verified {
			state.verifiedAt = now
			if targetHost != "" {
				if state.verifiedHosts == nil {
					state.verifiedHosts = make(map[string]time.Time)
				}
				state.verifiedHosts[targetHost] = now
			}
		}
		delete(p.failedByHost[strings.ToLower(strings.TrimSpace(targetHost))], key)
		return
	}
}

func (p *ProxyPool) MarkFailure(key, targetHost string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, state := range p.proxies {
		if state.key != key {
			continue
		}

		now := time.Now()
		state.healthy = false
		state.lastChecked = now
		state.verifiedAt = time.Time{}
		state.verifiedHosts = nil
		rotationKey := strings.ToLower(strings.TrimSpace(targetHost))
		if rotationKey != "" {
			if p.failedByHost == nil {
				p.failedByHost = make(map[string]map[string]bool)
			}
			if p.failedByHost[rotationKey] == nil {
				p.failedByHost[rotationKey] = make(map[string]bool)
			}
			p.failedByHost[rotationKey][key] = true
		}
		return
	}
}

func (p *ProxyPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.proxies)
}

func (p *ProxyPool) Replace(proxies []*proxyState) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.proxies = cloneProxyStates(proxies)
	if len(p.proxies) == 0 {
		p.next = 0
		return
	}
	p.next %= len(p.proxies)
}

func hasVerifiedHost(state *proxyState, targetHost string) bool {
	if state == nil || targetHost == "" || len(state.verifiedHosts) == 0 {
		return false
	}

	_, ok := state.verifiedHosts[targetHost]
	return ok
}

// ── Rotating proxy transport ──────────────────────────────────────────

type RotatingProxyTransport struct {
	logger    *log.Logger
	pool      *ProxyPool
	transport http.RoundTripper
}

func NewRotatingProxyTransport(pool *ProxyPool) *RotatingProxyTransport {
	logger := log.Default()
	if pool != nil && pool.logger != nil {
		logger = pool.logger
	}

	return &RotatingProxyTransport{
		logger:    logger,
		pool:      pool,
		transport: newProxyAwareTransport(),
	}
}

func (t *RotatingProxyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, hasBody, err := snapshotRequestBody(req)
	if err != nil {
		return nil, err
	}

	targetHost := requestTargetHost(req)
	return t.roundTripViaProxy(req, body, hasBody, targetHost)
}

func (t *RotatingProxyTransport) roundTripViaProxy(req *http.Request, body []byte, hasBody bool, targetHost string) (*http.Response, error) {
	if t.pool == nil {
		return nil, errNoUpstreamProxy
	}

	logger := t.transportLogger()
	targetLog := requestTargetLog(req)
	now := time.Now()
	candidates := t.pool.Candidates(now, targetHost)
	if len(candidates) == 0 {
		return nil, errNoUpstreamProxy
	}

	var lastErr error
	for _, candidate := range candidates {
		attemptReq := cloneRequestForProxy(req, candidate.url, body, hasBody)
		resp, err := t.transport.RoundTrip(attemptReq)
		if err != nil {
			t.pool.MarkFailure(candidate.key, targetHost)
			logger.Printf("[ERR] %s -> %s (%v)", targetLog, candidateLogAddress(candidate), err)
			lastErr = err
			continue
		}

		if shouldRetryStatus(resp.StatusCode) {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			t.pool.MarkFailure(candidate.key, targetHost)
			logger.Printf("[RETRY] %s -> %s (%d)", targetLog, candidateLogAddress(candidate), resp.StatusCode)
			lastErr = fmt.Errorf("origin returned retriable status %d via %s", resp.StatusCode, candidate.key)
			continue
		}

		t.pool.MarkSuccess(candidate.key, targetHost)
		logger.Printf("[OK] %s -> %s (%d)", targetLog, candidateLogAddress(candidate), resp.StatusCode)
		return resp, nil
	}

	if lastErr == nil {
		lastErr = errNoUpstreamProxy
	}

	return nil, lastErr
}

// DialContext dials a raw TCP connection through the upstream SOCKS proxy pool.
func (t *RotatingProxyTransport) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	targetHost := extractHost(addr)

	now := time.Now()
	candidates := t.pool.Candidates(now, targetHost)

	logger := t.transportLogger()

	if len(candidates) > 0 {
		for _, candidate := range candidates {
			if candidate.dialContext == nil {
				continue
			}

			conn, err := candidate.dialContext(ctx, network, addr)
			if err != nil {
				t.pool.MarkFailure(candidate.key, targetHost)
				logger.Printf("[ERR] CONNECT %s -> %s (%v)", addr, candidateLogAddress(candidate), err)
				continue
			}

			t.pool.MarkSuccess(candidate.key, targetHost)
			logger.Printf("[OK] CONNECT %s -> %s", addr, candidateLogAddress(candidate))
			return conn, nil
		}
	}

	// Fallback to direct connection
	logger.Printf("[DIRECT] CONNECT %s (no proxy)", addr)
	return (&net.Dialer{Timeout: proxyDialTimeout}).DialContext(ctx, network, addr)
}

func extractHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return strings.ToLower(host)
}

// ── Logging helpers ───────────────────────────────────────────────────

func proxyLogAddress(proxyURL *url.URL) string {
	if proxyURL == nil {
		return "-"
	}

	host := proxyURL.Hostname()
	if host == "" {
		return proxyURL.Host
	}

	return host
}

func candidateLogAddress(c proxyCandidate) string {
	host := c.url.Hostname()
	if host == "" {
		host = c.url.Host
	}

	if c.country != "" {
		return fmt.Sprintf("%s (%s)", host, c.country)
	}

	return host
}

func (t *RotatingProxyTransport) transportLogger() *log.Logger {
	logger := t.logger
	if logger == nil {
		logger = log.Default()
		if t.pool != nil && t.pool.logger != nil {
			logger = t.pool.logger
		}
	}

	return logger
}

func requestTargetHost(req *http.Request) string {
	if req == nil || req.URL == nil {
		return ""
	}

	host := strings.ToLower(req.URL.Hostname())
	if host != "" {
		return host
	}

	return strings.ToLower(req.URL.Host)
}

func requestTargetLog(req *http.Request) string {
	if req == nil || req.URL == nil {
		return "-"
	}

	host := req.URL.Host
	if hostname := req.URL.Hostname(); hostname != "" {
		host = hostname
	}
	if host == "" {
		host = "-"
	}

	path := req.URL.EscapedPath()
	if path == "" || path == "/" {
		path = ""
	}
	if req.URL.RawQuery != "" {
		path += "?" + req.URL.RawQuery
	}

	return strings.ToLower(host) + path
}

// ── Transport helpers ─────────────────────────────────────────────────

type proxyContextKey struct{}

func newProxyAwareTransport() http.RoundTripper {
	dialer := &net.Dialer{
		Timeout:   proxyDialTimeout,
		KeepAlive: 30 * time.Second,
	}

	return &http.Transport{
		Proxy: func(req *http.Request) (*url.URL, error) {
			proxyURL, _ := req.Context().Value(proxyContextKey{}).(*url.URL)
			return proxyURL, nil
		},
		DialContext:           dialer.DialContext,
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: proxyHeaderTimeout,
		ExpectContinueTimeout: time.Second,
	}
}

func snapshotRequestBody(req *http.Request) ([]byte, bool, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, false, nil
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, true, err
	}
	if err := req.Body.Close(); err != nil {
		return nil, true, err
	}

	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	req.ContentLength = int64(len(body))

	return body, true, nil
}

func cloneRequestForProxy(req *http.Request, proxyURL *url.URL, body []byte, hasBody bool) *http.Request {
	ctx := req.Context()
	if proxyURL != nil {
		ctx = context.WithValue(ctx, proxyContextKey{}, proxyURL)
	}

	attemptReq := req.Clone(ctx)

	if !hasBody {
		attemptReq.Body = nil
		attemptReq.GetBody = nil
		return attemptReq
	}

	attemptReq.Body = io.NopCloser(bytes.NewReader(body))
	attemptReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	attemptReq.ContentLength = int64(len(body))

	return attemptReq
}

func appendCandidatesByProtocolPriority(dst []proxyCandidate, candidates []proxyCandidate) []proxyCandidate {
	if len(candidates) == 0 {
		return dst
	}

	socks5 := make([]proxyCandidate, 0, len(candidates))
	socks4 := make([]proxyCandidate, 0, len(candidates))
	httpCandidates := make([]proxyCandidate, 0, len(candidates))
	other := make([]proxyCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		switch proxySchemePriority(candidate.url) {
		case 0:
			socks5 = append(socks5, candidate)
		case 1:
			socks4 = append(socks4, candidate)
		case 2:
			httpCandidates = append(httpCandidates, candidate)
		default:
			other = append(other, candidate)
		}
	}

	dst = append(dst, socks5...)
	dst = append(dst, socks4...)
	dst = append(dst, httpCandidates...)
	dst = append(dst, other...)
	return dst
}

func proxySchemePriority(u *url.URL) int {
	if u == nil {
		return 3
	}

	switch strings.ToLower(strings.TrimSpace(u.Scheme)) {
	case "socks5", "socks5h":
		return 0
	case "socks4", "socks4a":
		return 1
	case "https":
		return 2
	case "http":
		return 3
	default:
		return 4
	}
}

func shouldRetryStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests
}

func cloneProxyStates(proxies []*proxyState) []*proxyState {
	if len(proxies) == 0 {
		return nil
	}

	cloned := make([]*proxyState, 0, len(proxies))
	for _, proxy := range proxies {
		if proxy == nil || proxy.url == nil {
			continue
		}

		state := *proxy
		state.url = cloneURL(proxy.url)
		state.verifiedHosts = cloneVerifiedHosts(proxy.verifiedHosts)
		cloned = append(cloned, &state)
	}

	return cloned
}

func cloneVerifiedHosts(verifiedHosts map[string]time.Time) map[string]time.Time {
	if len(verifiedHosts) == 0 {
		return nil
	}

	cloned := make(map[string]time.Time, len(verifiedHosts))
	for host, verifiedAt := range verifiedHosts {
		cloned[host] = verifiedAt
	}

	return cloned
}

func cloneURL(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}

	cloned := *u
	return &cloned
}
