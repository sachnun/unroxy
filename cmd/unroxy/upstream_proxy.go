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
			p.logger.Printf("proxy list refresh failed, using cached list from %s: %v", lastRefresh.Format(time.RFC3339), err)
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

func (p *ProxyPool) Candidates(now time.Time) []proxyCandidate {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.proxies) == 0 {
		return nil
	}

	start := p.next
	p.next = (p.next + 1) % len(p.proxies)

	preferred := make([]proxyCandidate, 0, len(p.proxies))
	fallback := make([]proxyCandidate, 0, len(p.proxies))

	for i := 0; i < len(p.proxies); i++ {
		state := p.proxies[(start+i)%len(p.proxies)]

		candidate := proxyCandidate{key: state.key, url: cloneURL(state.url)}
		if now.Before(state.unavailableUntil) {
			fallback = append(fallback, candidate)
		} else {
			preferred = append(preferred, candidate)
		}
	}

	if len(preferred) == 0 {
		return fallback
	}

	return append(preferred, fallback...)
}

func (p *ProxyPool) MarkSuccess(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, state := range p.proxies {
		if state.key != key {
			continue
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

		state.unavailableUntil = time.Now().Add(p.failureCooldown)
		return
	}
}

func (p *ProxyPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.proxies)
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

	candidates := t.pool.Candidates(now)
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
			if !shouldRetryError(err) {
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

		t.pool.MarkSuccess(candidate.key)
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

func shouldRetryError(err error) bool {
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
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
	_, ok := allowedProtocols[protocol]
	return ok
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

func cloneURL(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}

	cloned := *u
	return &cloned
}
