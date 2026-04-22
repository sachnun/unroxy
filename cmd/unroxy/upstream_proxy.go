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
	"time"
)

const (
	upstreamProxyListURL = "https://cdn.jsdelivr.net/gh/proxifly/free-proxy-list@main/proxies/all/data.json"
	upstreamProxyListTTL = 10 * time.Minute
	proxyFailureCooldown = time.Minute
	proxyFetchTimeout    = 30 * time.Second
	proxyDialTimeout     = 5 * time.Second
	proxyHeaderTimeout   = 20 * time.Second
	proxyMaintenanceTick = 30 * time.Second
	proxyHealthTimeout   = 8 * time.Second
	proxyHealthBatchSize = 16
	proxyHealthWorkers   = 4
	proxyHealthCheckURL  = "https://www.gstatic.com/generate_204"
)

var errNoUpstreamProxy = errors.New("no upstream proxies available")

type upstreamProxyEntry struct {
	Proxy    string `json:"proxy"`
	Protocol string `json:"protocol"`
	HTTPS    bool   `json:"https"`
}

type proxyState struct {
	key              string
	url              *url.URL
	unavailableUntil time.Time
	healthy          bool
	lastChecked      time.Time
	verifiedAt       time.Time
	verifiedHosts    map[string]time.Time
}

type proxyCandidate struct {
	key string
	url *url.URL
}

type ProxyPool struct {
	client           *http.Client
	logger           *log.Logger
	sourceURL        string
	failureCooldown  time.Duration
	allowedProtocols map[string]struct{}
	maintenanceTick  time.Duration
	healthTimeout    time.Duration
	healthBatchSize  int
	healthWorkers    int
	healthCheckURL   string
	healthTransport  http.RoundTripper

	refreshMu      sync.Mutex
	backgroundOnce sync.Once
	mu             sync.RWMutex
	proxies        []*proxyState
	lastRefresh    time.Time
	next           int
	healthNext     int
}

func NewProxyPool(logger *log.Logger, allowedProtocols map[string]struct{}) *ProxyPool {
	if logger == nil {
		logger = log.Default()
	}

	return &ProxyPool{
		client:           &http.Client{Timeout: proxyFetchTimeout},
		logger:           logger,
		sourceURL:        upstreamProxyListURL,
		failureCooldown:  proxyFailureCooldown,
		allowedProtocols: cloneAllowedProtocols(allowedProtocols),
		maintenanceTick:  proxyMaintenanceTick,
		healthTimeout:    proxyHealthTimeout,
		healthBatchSize:  proxyHealthBatchSize,
		healthWorkers:    proxyHealthWorkers,
		healthCheckURL:   proxyHealthCheckURL,
		healthTransport:  newProxyAwareTransport(),
	}
}

func (p *ProxyPool) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.sourceURL, nil)
	if err != nil {
		return err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected proxy list status: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	states, err := parseProxyStates(body, p.allowedProtocols)
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	previous := make(map[string]*proxyState, len(p.proxies))
	for _, state := range p.proxies {
		previous[state.key] = state
	}

	for _, state := range states {
		if old, ok := previous[state.key]; ok {
			state.unavailableUntil = old.unavailableUntil
			state.healthy = old.healthy
			state.lastChecked = old.lastChecked
			state.verifiedAt = old.verifiedAt
			state.verifiedHosts = cloneVerifiedHosts(old.verifiedHosts)
		}
	}

	sort.Slice(states, func(i, j int) bool {
		return states[i].key < states[j].key
	})

	p.proxies = states
	p.lastRefresh = time.Now()
	if len(states) == 0 {
		p.next = 0
		return nil
	}

	if p.next >= len(states) {
		p.next = 0
	}
	if p.healthNext >= len(states) {
		p.healthNext = 0
	}

	return nil
}

func (p *ProxyPool) StartBackgroundMaintenance(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}

	p.backgroundOnce.Do(func() {
		go func() {
			if err := p.MaintainOnce(ctx, time.Now()); err != nil {
				p.logf("proxy maintenance failed: %v", err)
			}

			ticker := time.NewTicker(p.maintenanceTick)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case now := <-ticker.C:
					if err := p.MaintainOnce(ctx, now); err != nil {
						p.logf("proxy maintenance failed: %v", err)
					}
				}
			}
		}()
	})
}

func (p *ProxyPool) MaintainOnce(ctx context.Context, now time.Time) error {
	if err := p.EnsureFresh(ctx, now); err != nil {
		return err
	}

	checked, healthy, failed := p.checkProxyHealth(ctx, now)
	if checked > 0 {
		probeHealthy, verified := p.healthSummary()
		p.logf("proxy maintenance batch=%d ok=%d failed=%d probe_healthy=%d verified=%d pool=%d", checked, healthy, failed, probeHealthy, verified, p.Count())
	}

	return nil
}

func (p *ProxyPool) EnsureFresh(ctx context.Context, now time.Time) error {
	if !p.needsRefresh(now) {
		return nil
	}

	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()

	if !p.needsRefresh(now) {
		return nil
	}

	if err := p.Refresh(ctx); err != nil {
		p.mu.RLock()
		hasCachedProxies := len(p.proxies) > 0
		lastRefresh := p.lastRefresh
		p.mu.RUnlock()

		if hasCachedProxies {
			p.logf("proxy list refresh failed, using cached list from %s: %v", lastRefresh.Format(time.RFC3339), err)
			return nil
		}

		return err
	}

	return nil
}

func (p *ProxyPool) needsRefresh(now time.Time) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.lastRefresh.IsZero() || now.Sub(p.lastRefresh) >= upstreamProxyListTTL
}

func (p *ProxyPool) Candidates(now time.Time, targetHost string) []proxyCandidate {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.proxies) == 0 {
		return nil
	}

	start := p.next
	p.next = (p.next + 1) % len(p.proxies)

	hostVerified := make([]proxyCandidate, 0, len(p.proxies))
	verified := make([]proxyCandidate, 0, len(p.proxies))
	probed := make([]proxyCandidate, 0, len(p.proxies))
	untested := make([]proxyCandidate, 0, len(p.proxies))
	retry := make([]proxyCandidate, 0, len(p.proxies))
	cooling := make([]proxyCandidate, 0, len(p.proxies))

	for i := 0; i < len(p.proxies); i++ {
		state := p.proxies[(start+i)%len(p.proxies)]

		candidate := proxyCandidate{key: state.key, url: cloneURL(state.url)}
		if now.Before(state.unavailableUntil) {
			cooling = append(cooling, candidate)
			continue
		}

		switch {
		case hasVerifiedHost(state, targetHost):
			hostVerified = append(hostVerified, candidate)
		case !state.verifiedAt.IsZero():
			verified = append(verified, candidate)
		case state.healthy:
			probed = append(probed, candidate)
		case state.lastChecked.IsZero():
			untested = append(untested, candidate)
		default:
			retry = append(retry, candidate)
		}
	}

	ordered := make([]proxyCandidate, 0, len(p.proxies))
	ordered = append(ordered, hostVerified...)
	ordered = append(ordered, verified...)
	ordered = append(ordered, probed...)
	ordered = append(ordered, untested...)
	ordered = append(ordered, retry...)
	ordered = append(ordered, cooling...)
	return ordered
}

func (p *ProxyPool) MarkSuccess(key, targetHost string) {
	p.markSuccess(key, targetHost, true)
}

func (p *ProxyPool) MarkProbeSuccess(key string) {
	p.markSuccess(key, "", false)
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
		state.unavailableUntil = time.Time{}
		return
	}
}

func (p *ProxyPool) MarkFailure(key string) {
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
		state.unavailableUntil = now.Add(p.failureCooldown)
		return
	}
}

func (p *ProxyPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.proxies)
}

func (p *ProxyPool) logf(format string, args ...any) {
	logger := p.logger
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf(format, args...)
}

func (p *ProxyPool) healthSummary() (int, int) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	probeHealthy := 0
	verified := 0
	for _, state := range p.proxies {
		if state.healthy {
			probeHealthy++
		}
		if !state.verifiedAt.IsZero() {
			verified++
		}
	}

	return probeHealthy, verified
}

func hasVerifiedHost(state *proxyState, targetHost string) bool {
	if state == nil || targetHost == "" || len(state.verifiedHosts) == 0 {
		return false
	}

	_, ok := state.verifiedHosts[targetHost]
	return ok
}

func (p *ProxyPool) HealthCheckCandidates(now time.Time) []proxyCandidate {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.proxies) == 0 || p.healthBatchSize <= 0 {
		return nil
	}

	start := p.healthNext
	advance := p.healthBatchSize
	if advance > len(p.proxies) {
		advance = len(p.proxies)
	}
	p.healthNext = (p.healthNext + advance) % len(p.proxies)

	unchecked := make([]proxyCandidate, 0, p.healthBatchSize)
	retry := make([]proxyCandidate, 0, p.healthBatchSize)
	staleHealthy := make([]proxyCandidate, 0, p.healthBatchSize)

	for i := 0; i < len(p.proxies); i++ {
		state := p.proxies[(start+i)%len(p.proxies)]
		if now.Before(state.unavailableUntil) {
			continue
		}

		candidate := proxyCandidate{key: state.key, url: cloneURL(state.url)}
		switch {
		case state.lastChecked.IsZero():
			unchecked = append(unchecked, candidate)
		case !state.healthy:
			retry = append(retry, candidate)
		case now.Sub(state.lastChecked) >= p.maintenanceTick:
			staleHealthy = append(staleHealthy, candidate)
		}
	}

	selected := make([]proxyCandidate, 0, p.healthBatchSize)
	for _, group := range [][]proxyCandidate{unchecked, retry, staleHealthy} {
		for _, candidate := range group {
			selected = append(selected, candidate)
			if len(selected) == p.healthBatchSize {
				return selected
			}
		}
	}

	return selected
}

func (p *ProxyPool) checkProxyHealth(ctx context.Context, now time.Time) (int, int, int) {
	candidates := p.HealthCheckCandidates(now)
	if len(candidates) == 0 {
		return 0, 0, 0
	}

	workers := p.healthWorkers
	if workers < 1 {
		workers = 1
	}

	transport := p.healthTransport
	if transport == nil {
		transport = newProxyAwareTransport()
	}

	results := make(chan bool, len(candidates))
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)

	for _, candidate := range candidates {
		wg.Add(1)
		sem <- struct{}{}

		go func(candidate proxyCandidate) {
			defer wg.Done()
			defer func() {
				<-sem
			}()

			healthy, checked := p.checkProxyCandidate(ctx, transport, candidate)
			if !checked {
				return
			}

			results <- healthy
		}(candidate)
	}

	wg.Wait()
	close(results)

	healthy := 0
	failed := 0
	for result := range results {
		if result {
			healthy++
			continue
		}

		failed++
	}

	return healthy + failed, healthy, failed
}

func (p *ProxyPool) checkProxyCandidate(ctx context.Context, transport http.RoundTripper, candidate proxyCandidate) (bool, bool) {
	if ctx != nil && ctx.Err() != nil {
		return false, false
	}

	checkCtx := context.Background()
	if ctx != nil {
		checkCtx = ctx
	}

	checkCtx, cancel := context.WithTimeout(checkCtx, p.healthTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, p.healthCheckURL, nil)
	if err != nil {
		return false, false
	}

	req = cloneRequestForProxy(req, candidate.url, nil, false)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		if ctx != nil && ctx.Err() != nil {
			return false, false
		}

		p.MarkFailure(candidate.key)
		return false, true
	}
	defer resp.Body.Close()

	io.Copy(io.Discard, resp.Body)
	if isHealthyProxyStatus(resp.StatusCode) {
		p.MarkProbeSuccess(candidate.key)
		return true, true
	}

	p.MarkFailure(candidate.key)
	return false, true
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
	logger := t.logger
	if logger == nil {
		logger = log.Default()
		if t.pool != nil && t.pool.logger != nil {
			logger = t.pool.logger
		}
	}

	now := time.Now()
	if err := t.pool.EnsureFresh(req.Context(), now); err != nil {
		return nil, err
	}

	targetHost := req.URL.Host
	candidates := t.pool.Candidates(now, targetHost)
	if len(candidates) == 0 {
		return nil, errNoUpstreamProxy
	}

	body, hasBody, err := snapshotRequestBody(req)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for _, candidate := range candidates {
		logger.Printf("proxy attempt target=%s via=%s", req.URL.String(), candidate.key)
		attemptReq := cloneRequestForProxy(req, candidate.url, body, hasBody)
		resp, err := t.transport.RoundTrip(attemptReq)
		if err != nil {
			if !shouldRetryError(req.Context(), err) {
				logger.Printf("proxy failed target=%s via=%s err=%v", req.URL.String(), candidate.key, err)
				return nil, err
			}

			t.pool.MarkFailure(candidate.key)
			logger.Printf("proxy failed target=%s via=%s err=%v", req.URL.String(), candidate.key, err)
			lastErr = err
			continue
		}

		if shouldRetryStatus(resp.StatusCode) {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			t.pool.MarkFailure(candidate.key)
			logger.Printf("proxy retry status target=%s via=%s status=%d", req.URL.String(), candidate.key, resp.StatusCode)
			lastErr = fmt.Errorf("origin returned retriable status %d via %s", resp.StatusCode, candidate.key)
			continue
		}

		t.pool.MarkSuccess(candidate.key, targetHost)
		logger.Printf("proxy success target=%s via=%s status=%d", req.URL.String(), candidate.key, resp.StatusCode)
		return resp, nil
	}

	if lastErr == nil {
		lastErr = errNoUpstreamProxy
	}

	return nil, lastErr
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
		ForceAttemptHTTP2:     true,
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
	ctx := context.WithValue(req.Context(), proxyContextKey{}, proxyURL)
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
	return statusCode == http.StatusForbidden || statusCode == http.StatusTooManyRequests
}

func isHealthyProxyStatus(statusCode int) bool {
	return statusCode >= http.StatusOK && statusCode < http.StatusBadRequest
}

func shouldRetryError(ctx context.Context, err error) bool {
	if ctx != nil && ctx.Err() != nil {
		return false
	}

	return !errors.Is(err, context.Canceled)
}

func parseProxyStates(body []byte, allowedProtocols map[string]struct{}) ([]*proxyState, error) {
	var entries []upstreamProxyEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, err
	}

	states := make([]*proxyState, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if !isAllowedProxyEntry(entry, allowedProtocols) {
			continue
		}

		parsed, err := url.Parse(strings.TrimSpace(entry.Proxy))
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			continue
		}
		if !supportsProxyScheme(parsed.Scheme) {
			continue
		}

		key := parsed.String()
		if _, ok := seen[key]; ok {
			continue
		}

		seen[key] = struct{}{}
		states = append(states, &proxyState{key: key, url: parsed})
	}

	return states, nil
}

func isAllowedProxyEntry(entry upstreamProxyEntry, allowedProtocols map[string]struct{}) bool {
	if len(allowedProtocols) == 0 {
		return false
	}

	protocol := strings.ToLower(strings.TrimSpace(entry.Protocol))
	switch protocol {
	case "http":
		if !entry.HTTPS {
			return false
		}
		_, ok := allowedProtocols["http"]
		return ok
	case "https":
		_, ok := allowedProtocols["https"]
		return ok
	case "socks5", "socks5h":
		_, ok := allowedProtocols["socks5"]
		return ok
	default:
		return false
	}
}

func supportsProxyScheme(scheme string) bool {
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "http", "https", "socks5", "socks5h":
		return true
	default:
		return false
	}
}

func cloneAllowedProtocols(allowedProtocols map[string]struct{}) map[string]struct{} {
	if len(allowedProtocols) == 0 {
		return nil
	}

	cloned := make(map[string]struct{}, len(allowedProtocols))
	for protocol := range allowedProtocols {
		cloned[protocol] = struct{}{}
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
