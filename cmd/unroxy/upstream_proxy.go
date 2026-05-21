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
)

const (
	proxyFailureCooldown = time.Minute
	proxyDialTimeout     = 5 * time.Second
	proxyHeaderTimeout   = 20 * time.Second
	webshareRefreshEvery = 6 * time.Hour
	webshareAPIKeyEnv    = "API_KEY"
)

var (
	errNoUpstreamProxy    = errors.New("no upstream proxies available")
	websharePlansURL      = "https://proxy.webshare.io/api/v2/subscription/plan/"
	webshareProxyListURL  = "https://proxy.webshare.io/api/v2/proxy/list/"
	webshareAPIHTTPClient = &http.Client{Timeout: 30 * time.Second}
)

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

func NewWebshareProxyPool(logger *log.Logger, apiKeyValue string) (*ProxyPool, error) {
	apiKeys, err := parseWebshareAPIKeys(apiKeyValue)
	if err != nil {
		return nil, err
	}

	proxies, err := fetchWebshareProxyStates(webshareAPIHTTPClient, apiKeys)
	if err != nil {
		return nil, err
	}
	pool := NewProxyPool(logger, proxies)
	startWebshareProxyRefresh(pool, apiKeys)
	return pool, nil
}

func startWebshareProxyRefresh(pool *ProxyPool, apiKeys []string) {
	if pool == nil || len(apiKeys) == 0 {
		return
	}

	keys := append([]string(nil), apiKeys...)
	go func() {
		ticker := time.NewTicker(webshareRefreshEvery)
		defer ticker.Stop()

		for range ticker.C {
			proxies, err := fetchWebshareProxyStates(webshareAPIHTTPClient, keys)
			if err != nil {
				pool.logger.Printf("Webshare proxy refresh failed")
				continue
			}

			pool.Replace(proxies)
			pool.logger.Printf("Webshare proxy refreshed: %d proxies", pool.Count())
		}
	}()
}

func fetchWebshareProxyStates(client *http.Client, apiKeys []string) ([]*proxyState, error) {
	proxies := make([]*proxyState, 0)
	for _, apiKey := range apiKeys {
		states, err := fetchWebshareFreeDirectProxyStates(client, apiKey, len(proxies))
		if err != nil {
			return nil, err
		}
		proxies = append(proxies, states...)
	}
	if len(proxies) == 0 {
		return nil, errors.New("Webshare free direct proxy list is empty")
	}

	return proxies, nil
}

func parseWebshareAPIKeys(apiKeyValue string) ([]string, error) {
	apiKeyValue = strings.TrimSpace(apiKeyValue)
	if apiKeyValue == "" {
		return nil, fmt.Errorf("%s must be set", webshareAPIKeyEnv)
	}

	parts := strings.Split(apiKeyValue, ",")
	apiKeys := parts[:0]
	for _, apiKey := range parts {
		apiKey = strings.TrimSpace(apiKey)
		if apiKey != "" {
			apiKeys = append(apiKeys, apiKey)
		}
	}
	if len(apiKeys) == 0 {
		return nil, fmt.Errorf("%s must contain at least one API key", webshareAPIKeyEnv)
	}

	return apiKeys, nil
}

type websharePlan struct {
	ID        int    `json:"id"`
	Status    string `json:"status"`
	ProxyType string `json:"proxy_type"`
}

type websharePlanListResponse struct {
	Results []websharePlan `json:"results"`
}

type webshareProxy struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	ProxyAddress string `json:"proxy_address"`
	Port         int    `json:"port"`
}

type webshareProxyListResponse struct {
	Next    string          `json:"next"`
	Results []webshareProxy `json:"results"`
}

func fetchWebshareFreeDirectProxyStates(client *http.Client, apiKey string, keyOffset int) ([]*proxyState, error) {
	plans, err := fetchWebshareFreePlans(client, apiKey)
	if err != nil {
		return nil, err
	}

	proxies := make([]*proxyState, 0)
	for _, plan := range plans {
		states, err := fetchWebshareDirectProxyStates(client, apiKey, plan.ID, keyOffset+len(proxies))
		if err != nil {
			return nil, err
		}
		proxies = append(proxies, states...)
	}

	return proxies, nil
}

func fetchWebshareFreePlans(client *http.Client, apiKey string) ([]websharePlan, error) {
	var response websharePlanListResponse
	if err := webshareGetJSON(client, websharePlansURL+"?page_size=100", apiKey, &response); err != nil {
		return nil, fmt.Errorf("fetch Webshare plans: %w", err)
	}

	plans := make([]websharePlan, 0, len(response.Results))
	for _, plan := range response.Results {
		if strings.EqualFold(plan.Status, "active") && strings.EqualFold(plan.ProxyType, "free") {
			plans = append(plans, plan)
		}
	}

	return plans, nil
}

func fetchWebshareDirectProxyStates(client *http.Client, apiKey string, planID, keyOffset int) ([]*proxyState, error) {
	proxyListURL, err := url.Parse(webshareProxyListURL)
	if err != nil {
		return nil, fmt.Errorf("parse Webshare proxy list URL: %w", err)
	}

	query := proxyListURL.Query()
	query.Set("mode", "direct")
	query.Set("page", "1")
	query.Set("page_size", "100")
	query.Set("plan_id", fmt.Sprint(planID))
	proxyListURL.RawQuery = query.Encode()

	proxies := make([]*proxyState, 0)
	nextURL := proxyListURL.String()
	for nextURL != "" {
		var response webshareProxyListResponse
		if err := webshareGetJSON(client, nextURL, apiKey, &response); err != nil {
			return nil, fmt.Errorf("fetch Webshare direct proxies: %w", err)
		}

		states, err := webshareProxyStatesFromAPI(response.Results, keyOffset+len(proxies))
		if err != nil {
			return nil, err
		}
		proxies = append(proxies, states...)
		nextURL = response.Next
	}

	return proxies, nil
}

func webshareGetJSON(client *http.Client, rawURL, apiKey string, out any) error {
	if client == nil {
		client = http.DefaultClient
	}

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Token "+apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("Webshare API returned status %d", resp.StatusCode)
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

func webshareProxyStatesFromAPI(apiProxies []webshareProxy, keyOffset int) ([]*proxyState, error) {
	proxies := make([]*proxyState, 0, len(apiProxies))
	for _, apiProxy := range apiProxies {
		host := strings.TrimSpace(apiProxy.ProxyAddress)
		if host == "" {
			return nil, errors.New("invalid Webshare proxy API response")
		}

		port := apiProxy.Port
		if port == 0 {
			return nil, errors.New("invalid Webshare proxy API response")
		}

		username := strings.TrimSpace(apiProxy.Username)
		password := strings.TrimSpace(apiProxy.Password)
		if username == "" || password == "" {
			return nil, errors.New("invalid Webshare proxy API response")
		}

		hostPort := net.JoinHostPort(host, fmt.Sprint(port))
		proxyURL := &url.URL{
			Scheme: "socks5",
			User:   url.UserPassword(username, password),
			Host:   hostPort,
		}
		keyID := apiProxy.ID
		if keyID == "" {
			keyID = fmt.Sprint(keyOffset + len(proxies) + 1)
		}
		proxies = append(proxies, &proxyState{
			key: fmt.Sprintf("socks5://%s#%s", hostPort, keyID),
			url: proxyURL,
		})
	}

	return proxies, nil
}

func (p *ProxyPool) Candidates(now time.Time, targetHost string) []proxyCandidate {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.proxies) == 0 {
		return nil
	}

	start := p.next
	p.next = (p.next + 1) % len(p.proxies)

	ready := make([]proxyCandidate, 0, len(p.proxies))
	cooling := make([]proxyCandidate, 0, len(p.proxies))

	for i := 0; i < len(p.proxies); i++ {
		state := p.proxies[(start+i)%len(p.proxies)]

		candidate := proxyCandidate{key: state.key, url: cloneURL(state.url)}
		if now.Before(state.unavailableUntil) {
			cooling = append(cooling, candidate)
			continue
		}

		ready = append(ready, candidate)
	}

	ordered := make([]proxyCandidate, 0, len(p.proxies))
	ordered = append(ordered, ready...)
	ordered = append(ordered, cooling...)
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
			t.pool.MarkFailure(candidate.key)
			logger.Printf("[ERR] %s -> %s (%v)", targetHost, proxyLogAddress(candidate.url), err)
			return nil, err
		}

		if shouldRetryStatus(resp.StatusCode) {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			t.pool.MarkFailure(candidate.key)
			logger.Printf("[RETRY] %s -> %s (%d)", targetHost, proxyLogAddress(candidate.url), resp.StatusCode)
			lastErr = fmt.Errorf("origin returned retriable status %d via %s", resp.StatusCode, candidate.key)
			continue
		}

		t.pool.MarkSuccess(candidate.key, targetHost)
		logger.Printf("[OK] %s -> %s (%d)", targetHost, proxyLogAddress(candidate.url), resp.StatusCode)
		return resp, nil
	}

	if lastErr == nil {
		lastErr = errNoUpstreamProxy
	}

	return nil, lastErr
}

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
