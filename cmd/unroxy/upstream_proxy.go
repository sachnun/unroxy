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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"
	"h12.io/socks"
)

const (
	proxyDialTimeout       = 5 * time.Second
	proxyHeaderTimeout     = 20 * time.Second
	proxyHealthTimeout     = 3 * time.Second
	providerFetchTimeout   = 30 * time.Second
	providerRefreshEvery   = 5 * time.Minute
	healthCheckConcurrency = 300
)

type ProxyProvider interface {
	Name() string
	Fetch() ([]*proxyState, error)
	ETag() (string, error)
}

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
	key         string
	url         *url.URL
	country     string
	latency     time.Duration
	healthy     bool
	lastChecked time.Time
	priority    int
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

type proxyCandidate struct {
	key         string
	url         *url.URL
	country     string
	latency     time.Duration
	priority    int
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

type ProxyPool struct {
	logger *log.Logger

	mu           sync.RWMutex
	proxies      []*proxyState
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

type proxiflyProvider struct{}

func (p *proxiflyProvider) Name() string { return "Proxifly" }

func (p *proxiflyProvider) ETag() (string, error) {
	client := &http.Client{Timeout: providerFetchTimeout}
	var etags []string
	for _, path := range []string{
		"protocols/socks5/data.json",
		"protocols/socks4/data.json",
	} {
		req, err := http.NewRequest(http.MethodHead, proxiflyBaseURL+path, nil)
		if err != nil {
			return "", err
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		resp.Body.Close()
		et := resp.Header.Get("ETag")
		if et == "" {
			return "", fmt.Errorf("no ETag for %s", path)
		}
		etags = append(etags, et)
	}
	return strings.Join(etags, "|"), nil
}

func (p *proxiflyProvider) Fetch() ([]*proxyState, error) {
	return fetchProxiflyProxies()
}

func fetchProxiflyProxies() ([]*proxyState, error) {
	client := &http.Client{Timeout: providerFetchTimeout}

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

func groupProxiesByCountry(proxies []*proxyState) map[string][]*proxyState {
	groups := make(map[string][]*proxyState)
	for _, p := range proxies {
		code := p.country
		if code == "" {
			code = "XX"
		}
		groups[code] = append(groups[code], p)
	}
	return groups
}

func NewProxiflyCountryPools(logger *log.Logger) (map[string]*ProxyPool, []*proxyState, error) {
	proxies, err := fetchProxiflyProxies()
	if err != nil {
		return nil, nil, fmt.Errorf("proxifly=%w", err)
	}

	logger.Printf("Proxifly: %d proxies", len(proxies))
	proxies = testProxiesConcurrently(proxies, healthCheckConcurrency, logger)
	groups := groupProxiesByCountry(proxies)

	pools := make(map[string]*ProxyPool, len(groups))
	for country, states := range groups {
		pools[country] = NewProxyPool(logger, states)
	}

	return pools, proxies, nil
}

func startProxyRefresh(providers []ProxyProvider, countryPools map[string]*ProxyPool, defaultPool *ProxyPool, logger *log.Logger) {
	go func() {
		ticker := time.NewTicker(providerRefreshEvery)
		defer ticker.Stop()

		lastETags := make(map[string]string)
		for range ticker.C {
			for _, provider := range providers {
				name := provider.Name()
				etag, err := provider.ETag()
				if err != nil {
					logger.Printf("%s ETag check failed: %v", name, err)
					continue
				}
				if etag == lastETags[name] {
					logger.Printf("%s: no change", name)
					continue
				}

				proxies, err := provider.Fetch()
				if err != nil {
					logger.Printf("%s refresh failed: %v", name, err)
					continue
				}

				logger.Printf("%s: %d proxies", name, len(proxies))
				proxies = testProxiesConcurrently(proxies, healthCheckConcurrency, logger)
				groups := groupProxiesByCountry(proxies)

				defaultPool.Replace(proxies)

				for country, states := range groups {
					if pool, ok := countryPools[country]; ok {
						pool.Replace(states)
					}
				}

				lastETags[name] = etag
				logger.Printf("%s refreshed: %d healthy proxies", name, len(proxies))
			}
		}
	}()
}

func testProxiesConcurrently(proxies []*proxyState, concurrency int, logger *log.Logger) []*proxyState {
	if len(proxies) == 0 {
		return nil
	}

	sem := make(chan struct{}, concurrency)
	healthy := make([]*proxyState, 0, len(proxies))
	var mu sync.Mutex
	var wg sync.WaitGroup
	var tested int32
	total := len(proxies)

	for _, p := range proxies {
		sem <- struct{}{}
		wg.Add(1)
		go func(ps *proxyState) {
			defer wg.Done()
			defer func() { <-sem }()

			if testProxyReachable(ps) {
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

func testProxyReachable(p *proxyState) bool {
	if p.dialContext == nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), proxyHealthTimeout)
	defer cancel()

	start := time.Now()
	conn, err := p.dialContext(ctx, "tcp", "1.1.1.1:80")
	p.latency = time.Since(start)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (p *ProxyPool) Candidates(now time.Time, targetHost string) []proxyCandidate {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.proxies) == 0 {
		return nil
	}

	rotationKey := strings.ToLower(strings.TrimSpace(targetHost))
	failedKeys := p.failedByHost[rotationKey]

	ready := make([]proxyCandidate, 0, len(p.proxies))
	failed := make([]proxyCandidate, 0, len(p.proxies))

	for _, state := range p.proxies {
		if state == nil || state.url == nil {
			continue
		}

		candidate := proxyCandidate{
			key:         state.key,
			url:         cloneURL(state.url),
			country:     state.country,
			latency:     state.latency,
			priority:    state.priority,
			dialContext: state.dialContext,
		}

		if failedKeys[state.key] {
			failed = append(failed, candidate)
		} else {
			ready = append(ready, candidate)
		}
	}

	sort.SliceStable(ready, func(i, j int) bool {
		if ready[i].priority != ready[j].priority {
			return ready[i].priority < ready[j].priority
		}
		return ready[i].latency < ready[j].latency
	})
	sort.SliceStable(failed, func(i, j int) bool {
		if failed[i].priority != failed[j].priority {
			return failed[i].priority < failed[j].priority
		}
		return failed[i].latency < failed[j].latency
	})

	ordered := make([]proxyCandidate, 0, len(p.proxies))
	ordered = append(ordered, ready...)
	ordered = append(ordered, failed...)
	return ordered
}

func (p *ProxyPool) MarkSuccess(key, targetHost string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, state := range p.proxies {
		if state.key != key {
			continue
		}

		state.healthy = true
		state.lastChecked = time.Now()
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

		state.healthy = false
		state.lastChecked = time.Now()
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
	p.failedByHost = nil
}

func (p *ProxyPool) SetPrimary(primary *proxyState) {
	p.mu.Lock()
	defer p.mu.Unlock()

	cp := *primary
	cp.priority = 0
	p.proxies = append([]*proxyState{&cp}, p.proxies...)
}

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
			if isHostUnreachable(err) {
				t.pool.MarkFailure(candidate.key, targetHost)
				logger.Printf("[ERR] %s -> %s (%v)", targetLog, candidateLogAddress(candidate), err)
				lastErr = err
				break
			}
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

func (t *RotatingProxyTransport) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	targetHost := extractHost(addr)
	logger := t.transportLogger()

	now := time.Now()
	candidates := t.pool.Candidates(now, targetHost)

	if len(candidates) > 0 {
		for _, candidate := range candidates {
			if candidate.dialContext == nil {
				continue
			}

			conn, err := candidate.dialContext(ctx, network, addr)
			if err != nil {
				if isHostUnreachable(err) {
					t.pool.MarkFailure(candidate.key, targetHost)
					logger.Printf("[ERR] CONNECT %s -> %s (%v)", addr, candidateLogAddress(candidate), err)
					break
				}
				t.pool.MarkFailure(candidate.key, targetHost)
				logger.Printf("[ERR] CONNECT %s -> %s (%v)", addr, candidateLogAddress(candidate), err)
				continue
			}

			t.pool.MarkSuccess(candidate.key, targetHost)
			logger.Printf("[OK] CONNECT %s -> %s", addr, candidateLogAddress(candidate))
			return conn, nil
		}
	}

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
		DisableKeepAlives:     false,
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

func shouldRetryStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests
}

func isHostUnreachable(err error) bool {
	return strings.Contains(err.Error(), "host unreachable")
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
		cloned = append(cloned, &state)
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
