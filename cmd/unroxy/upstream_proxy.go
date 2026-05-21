package main

import (
	"bytes"
	"context"
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
)

const (
	proxyFailureCooldown = time.Minute
	proxyDialTimeout     = 5 * time.Second
	proxyHeaderTimeout   = 20 * time.Second
	webshareProxyHost    = "p.webshare.io"
	webshareProxyPort    = "80"
	webshareUsernameEnv  = "WEBSHARE_USERNAME"
	websharePasswordEnv  = "WEBSHARE_PASSWORD"
)

var errNoUpstreamProxy = errors.New("no upstream proxies available")

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
	logger          *log.Logger
	failureCooldown time.Duration

	mu      sync.RWMutex
	proxies []*proxyState
	next    int
}

func NewProxyPool(logger *log.Logger, proxies []*proxyState) *ProxyPool {
	if logger == nil {
		logger = log.Default()
	}

	return &ProxyPool{
		logger:          logger,
		failureCooldown: proxyFailureCooldown,
		proxies:         cloneProxyStates(proxies),
	}
}

func NewWebshareProxyPool(logger *log.Logger, username, password string) (*ProxyPool, error) {
	username = strings.TrimSpace(username)
	password = strings.TrimSpace(password)
	if username == "" || password == "" {
		return nil, fmt.Errorf("%s and %s must be set", webshareUsernameEnv, websharePasswordEnv)
	}

	proxyURL := &url.URL{
		Scheme: "socks5",
		User:   url.UserPassword(username, password),
		Host:   net.JoinHostPort(webshareProxyHost, webshareProxyPort),
	}
	state := &proxyState{
		key: "socks5://" + net.JoinHostPort(webshareProxyHost, webshareProxyPort),
		url: proxyURL,
	}

	return NewProxyPool(logger, []*proxyState{state}), nil
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

func hasVerifiedHost(state *proxyState, targetHost string) bool {
	if state == nil || targetHost == "" || len(state.verifiedHosts) == 0 {
		return false
	}

	_, ok := state.verifiedHosts[targetHost]
	return ok
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
			if !shouldRetryError(req.Context(), err) {
				logger.Printf("proxy_error web=%s error=%v", targetHost, err)
				return nil, err
			}

			t.pool.MarkFailure(candidate.key)
			logger.Printf("proxy_error web=%s error=%v", targetHost, err)
			lastErr = err
			continue
		}

		if shouldRetryStatus(resp.StatusCode) {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			t.pool.MarkFailure(candidate.key)
			logger.Printf("target_status web=%s status=%d", targetHost, resp.StatusCode)
			lastErr = fmt.Errorf("origin returned retriable status %d via %s", resp.StatusCode, candidate.key)
			continue
		}

		t.pool.MarkSuccess(candidate.key, targetHost)
		logger.Printf("connected web=%s ip=%s", targetHost, candidate.key)
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
