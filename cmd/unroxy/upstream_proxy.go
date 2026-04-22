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
	upstreamProxyListTTL = 10 * time.Minute
	restrictedHostTTL    = 10 * time.Minute
	proxyFailureCooldown = time.Minute
	proxyFetchTimeout    = 30 * time.Second
	proxyDialTimeout     = 5 * time.Second
	proxyHeaderTimeout   = 20 * time.Second
)

var upstreamProxyListURL = "https://cdn.jsdelivr.net/gh/proxifly/free-proxy-list@main/proxies/all/data.json"

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

	refreshMu   sync.Mutex
	mu          sync.RWMutex
	proxies     []*proxyState
	lastRefresh time.Time
	next        int
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
	}
}

func allowedProxyProtocols(protocols ...string) map[string]struct{} {
	if len(protocols) == 0 {
		return nil
	}

	allowed := make(map[string]struct{}, len(protocols))
	for _, protocol := range protocols {
		protocol = strings.ToLower(strings.TrimSpace(protocol))
		if protocol == "" {
			continue
		}

		allowed[protocol] = struct{}{}
	}

	if len(allowed) == 0 {
		return nil
	}

	return allowed
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
	ordered = appendCandidatesByProtocolPriority(ordered, hostVerified)
	ordered = appendCandidatesByProtocolPriority(ordered, verified)
	ordered = appendCandidatesByProtocolPriority(ordered, probed)
	ordered = appendCandidatesByProtocolPriority(ordered, untested)
	ordered = appendCandidatesByProtocolPriority(ordered, retry)
	ordered = appendCandidatesByProtocolPriority(ordered, cooling)
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

func hasVerifiedHost(state *proxyState, targetHost string) bool {
	if state == nil || targetHost == "" || len(state.verifiedHosts) == 0 {
		return false
	}

	_, ok := state.verifiedHosts[targetHost]
	return ok
}

type RotatingProxyTransport struct {
	logger            *log.Logger
	pool              *ProxyPool
	transport         http.RoundTripper
	restrictedHostTTL time.Duration

	mu              sync.Mutex
	restrictedHosts map[string]time.Time
}

func NewRotatingProxyTransport(pool *ProxyPool) *RotatingProxyTransport {
	logger := log.Default()
	if pool != nil && pool.logger != nil {
		logger = pool.logger
	}

	return &RotatingProxyTransport{
		logger:            logger,
		pool:              pool,
		transport:         newProxyAwareTransport(),
		restrictedHostTTL: restrictedHostTTL,
	}
}

func (t *RotatingProxyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, hasBody, err := snapshotRequestBody(req)
	if err != nil {
		return nil, err
	}

	targetHost := requestTargetHost(req)
	logger := t.transportLogger()
	if t.isRestrictedHost(targetHost, time.Now()) {
		logger.Printf("proxy fallback target=%s reason=host-restricted", req.URL.String())
		resp, err := t.roundTripViaProxy(req, body, hasBody, targetHost)
		if err == nil {
			return resp, nil
		}
		if req.Context().Err() != nil {
			return nil, err
		}

		resp, err = t.roundTripDirect(req, body, hasBody)
		if err != nil {
			return nil, err
		}
		if shouldRetryStatus(resp.StatusCode) {
			logger.Printf("direct retry status target=%s status=%d", req.URL.String(), resp.StatusCode)
			t.markRestrictedHost(targetHost, time.Now())
		} else {
			t.clearRestrictedHost(targetHost)
		}

		return resp, nil
	}

	directResp, err := t.roundTripDirect(req, body, hasBody)
	if err != nil {
		logger.Printf("direct failed target=%s err=%v", req.URL.String(), err)
		return nil, err
	}
	if !shouldRetryStatus(directResp.StatusCode) {
		t.clearRestrictedHost(targetHost)
		return directResp, nil
	}

	logger.Printf("direct retry status target=%s status=%d", req.URL.String(), directResp.StatusCode)
	t.markRestrictedHost(targetHost, time.Now())

	proxyResp, err := t.roundTripViaProxy(req, body, hasBody, targetHost)
	if err == nil {
		io.Copy(io.Discard, directResp.Body)
		directResp.Body.Close()
		return proxyResp, nil
	}

	return directResp, nil
}

func (t *RotatingProxyTransport) roundTripDirect(req *http.Request, body []byte, hasBody bool) (*http.Response, error) {
	attemptReq := cloneRequestForProxy(req, nil, body, hasBody)
	return t.transport.RoundTrip(attemptReq)
}

func (t *RotatingProxyTransport) roundTripViaProxy(req *http.Request, body []byte, hasBody bool, targetHost string) (*http.Response, error) {
	if t.pool == nil {
		return nil, errNoUpstreamProxy
	}

	logger := t.transportLogger()
	now := time.Now()
	if err := t.pool.EnsureFresh(req.Context(), now); err != nil {
		return nil, err
	}

	candidates := t.pool.Candidates(now, targetHost)
	if len(candidates) == 0 {
		return nil, errNoUpstreamProxy
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

func (t *RotatingProxyTransport) isRestrictedHost(targetHost string, now time.Time) bool {
	if targetHost == "" {
		return false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	until, ok := t.restrictedHosts[targetHost]
	if !ok {
		return false
	}
	if now.Before(until) {
		return true
	}

	delete(t.restrictedHosts, targetHost)
	return false
}

func (t *RotatingProxyTransport) markRestrictedHost(targetHost string, now time.Time) {
	if targetHost == "" {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.restrictedHosts == nil {
		t.restrictedHosts = make(map[string]time.Time)
	}
	t.restrictedHosts[targetHost] = now.Add(t.restrictedTTL())
}

func (t *RotatingProxyTransport) clearRestrictedHost(targetHost string) {
	if targetHost == "" {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.restrictedHosts == nil {
		return
	}
	delete(t.restrictedHosts, targetHost)
}

func (t *RotatingProxyTransport) restrictedTTL() time.Duration {
	if t.restrictedHostTTL > 0 {
		return t.restrictedHostTTL
	}

	return restrictedHostTTL
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

	socks := make([]proxyCandidate, 0, len(candidates))
	https := make([]proxyCandidate, 0, len(candidates))
	httpCandidates := make([]proxyCandidate, 0, len(candidates))
	other := make([]proxyCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		switch proxySchemePriority(candidate.url) {
		case 0:
			socks = append(socks, candidate)
		case 1:
			https = append(https, candidate)
		case 2:
			httpCandidates = append(httpCandidates, candidate)
		default:
			other = append(other, candidate)
		}
	}

	dst = append(dst, socks...)
	dst = append(dst, https...)
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
	case "https":
		return 1
	case "http":
		return 2
	default:
		return 3
	}
}

func shouldRetryStatus(statusCode int) bool {
	return statusCode == http.StatusForbidden || statusCode == http.StatusTooManyRequests
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
