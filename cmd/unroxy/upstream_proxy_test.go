package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestParseProxyStatesSockKeepsOnlySocks5(t *testing.T) {
	body := []byte(`[
		{"proxy":"socks5://1.1.1.1:1080","protocol":"socks5","https":false},
		{"proxy":"socks4://5.5.5.5:1080","protocol":"socks4","https":false},
		{"proxy":"http://2.2.2.2:8080","protocol":"http","https":true},
		{"proxy":"socks5://1.1.1.1:1080","protocol":"socks5","https":false}
	]`)

	states, err := parseProxyStates(body, parseProxyProtocols("sock"))
	if err != nil {
		t.Fatalf("parseProxyStates returned error: %v", err)
	}

	if len(states) != 1 {
		t.Fatalf("expected 1 state, got %d", len(states))
	}
	if states[0].key != "socks5://1.1.1.1:1080" {
		t.Fatalf("expected filtered list to keep socks5 proxy, got %q", states[0].key)
	}
}

func TestParseProxyStatesHTTPRequiresHTTPSSupport(t *testing.T) {
	body := []byte(`[
		{"proxy":"http://2.2.2.2:8080","protocol":"http","https":true},
		{"proxy":"http://3.3.3.3:8080","protocol":"http","https":false},
		{"proxy":"https://4.4.4.4:8443","protocol":"https","https":true},
		{"proxy":"socks5://1.1.1.1:1080","protocol":"socks5","https":false}
	]`)

	states, err := parseProxyStates(body, parseProxyProtocols("http"))
	if err != nil {
		t.Fatalf("parseProxyStates returned error: %v", err)
	}

	if len(states) != 2 {
		t.Fatalf("expected 2 states, got %d", len(states))
	}

	keys := []string{states[0].key, states[1].key}
	if !containsString(keys, "http://2.2.2.2:8080") {
		t.Fatalf("expected filtered list to keep https-capable http proxy, got %v", keys)
	}
	if !containsString(keys, "https://4.4.4.4:8443") {
		t.Fatalf("expected filtered list to keep https proxy, got %v", keys)
	}
	if containsString(keys, "http://3.3.3.3:8080") {
		t.Fatalf("expected non-https http proxy to be excluded, got %v", keys)
	}
}

func TestParseProxyStatesAllIncludesSupportedUsableProtocols(t *testing.T) {
	body := []byte(`[
		{"proxy":"socks5://1.1.1.1:1080","protocol":"socks5","https":false},
		{"proxy":"socks4://5.5.5.5:1080","protocol":"socks4","https":false},
		{"proxy":"http://2.2.2.2:8080","protocol":"http","https":true},
		{"proxy":"http://3.3.3.3:8080","protocol":"http","https":false},
		{"proxy":"https://4.4.4.4:8443","protocol":"https","https":true}
	]`)

	states, err := parseProxyStates(body, parseProxyProtocols("all"))
	if err != nil {
		t.Fatalf("parseProxyStates returned error: %v", err)
	}

	if len(states) != 3 {
		t.Fatalf("expected 3 states, got %d", len(states))
	}

	keys := []string{states[0].key, states[1].key, states[2].key}
	if containsString(keys, "socks4://5.5.5.5:1080") {
		t.Fatalf("expected socks4 proxy to be excluded, got %v", keys)
	}
	if containsString(keys, "http://3.3.3.3:8080") {
		t.Fatalf("expected plain http proxy without https support to be excluded, got %v", keys)
	}
}

func TestProxyPoolCandidatesRoundRobin(t *testing.T) {
	pool := &ProxyPool{
		proxies: []*proxyState{
			{key: "http://1.1.1.1:80", url: mustParseURL(t, "http://1.1.1.1:80")},
			{key: "http://2.2.2.2:80", url: mustParseURL(t, "http://2.2.2.2:80")},
			{key: "http://3.3.3.3:80", url: mustParseURL(t, "http://3.3.3.3:80")},
		},
	}

	first := pool.Candidates(time.Now(), "")
	if len(first) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(first))
	}
	if first[0].key != "http://1.1.1.1:80" || first[1].key != "http://2.2.2.2:80" || first[2].key != "http://3.3.3.3:80" {
		t.Fatalf("unexpected first candidate order: %q, %q, %q", first[0].key, first[1].key, first[2].key)
	}

	second := pool.Candidates(time.Now(), "")
	if second[0].key != "http://2.2.2.2:80" || second[1].key != "http://3.3.3.3:80" || second[2].key != "http://1.1.1.1:80" {
		t.Fatalf("unexpected second candidate order: %q, %q, %q", second[0].key, second[1].key, second[2].key)
	}
}

func TestProxyPoolCandidatesPreferHealthyThenUntestedThenRetryThenCooling(t *testing.T) {
	now := time.Now()
	pool := &ProxyPool{
		proxies: []*proxyState{
			{key: "http://verified:80", url: mustParseURL(t, "http://verified:80"), healthy: true, lastChecked: now.Add(-time.Minute), verifiedAt: now.Add(-30 * time.Second)},
			{key: "http://probed:80", url: mustParseURL(t, "http://probed:80"), healthy: true, lastChecked: now.Add(-time.Minute)},
			{key: "http://untested:80", url: mustParseURL(t, "http://untested:80")},
			{key: "http://retry:80", url: mustParseURL(t, "http://retry:80"), lastChecked: now.Add(-time.Minute)},
			{key: "http://cooling:80", url: mustParseURL(t, "http://cooling:80"), unavailableUntil: now.Add(time.Minute)},
		},
	}

	candidates := pool.Candidates(now, "")
	got := []string{candidates[0].key, candidates[1].key, candidates[2].key, candidates[3].key, candidates[4].key}
	want := []string{"http://verified:80", "http://probed:80", "http://untested:80", "http://retry:80", "http://cooling:80"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate order = %v, want %v", got, want)
		}
	}
}

func TestRotatingProxyTransportRetriesAttemptTimeoutWhenRequestContextAlive(t *testing.T) {
	pool := &ProxyPool{
		failureCooldown: time.Minute,
		lastRefresh:     time.Now(),
		proxies: []*proxyState{
			{key: "http://slow:80", url: mustParseURL(t, "http://slow:80")},
			{key: "http://good:80", url: mustParseURL(t, "http://good:80")},
		},
	}

	transport := &RotatingProxyTransport{
		pool: pool,
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			proxyURL, _ := req.Context().Value(proxyContextKey{}).(*url.URL)
			switch proxyURL.Host {
			case "slow:80":
				return nil, context.DeadlineExceeded
			case "good:80":
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("ok")),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			default:
				return nil, errors.New("unexpected proxy")
			}
		}),
	}

	req, err := http.NewRequest(http.MethodGet, "https://example.com/path", nil)
	if err != nil {
		t.Fatalf("http.NewRequest returned error: %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip returned error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 response, got %d", resp.StatusCode)
	}
	if pool.proxies[0].unavailableUntil.IsZero() {
		t.Fatalf("expected timeout proxy to enter cooldown")
	}
	if !pool.proxies[1].healthy || pool.proxies[1].verifiedAt.IsZero() {
		t.Fatalf("expected second proxy to be marked verified after real success")
	}
	if _, ok := pool.proxies[1].verifiedHosts["example.com"]; !ok {
		t.Fatalf("expected second proxy to be verified for example.com")
	}
}

func TestProxyPoolCandidatesPreferVerifiedHostFirst(t *testing.T) {
	now := time.Now()
	pool := &ProxyPool{
		proxies: []*proxyState{
			{key: "http://global:80", url: mustParseURL(t, "http://global:80"), healthy: true, lastChecked: now.Add(-time.Minute), verifiedAt: now.Add(-time.Minute)},
			{key: "http://host:80", url: mustParseURL(t, "http://host:80"), healthy: true, lastChecked: now.Add(-time.Minute), verifiedAt: now.Add(-time.Minute), verifiedHosts: map[string]time.Time{"opencode.ai": now.Add(-time.Minute)}},
			{key: "http://probed:80", url: mustParseURL(t, "http://probed:80"), healthy: true, lastChecked: now.Add(-time.Minute)},
		},
	}

	candidates := pool.Candidates(now, "opencode.ai")
	got := []string{candidates[0].key, candidates[1].key, candidates[2].key}
	want := []string{"http://host:80", "http://global:80", "http://probed:80"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate order = %v, want %v", got, want)
		}
	}
}

func TestRotatingProxyTransportRetriesUntilSuccess(t *testing.T) {
	var logs strings.Builder
	pool := &ProxyPool{
		logger:          log.New(&logs, "", 0),
		failureCooldown: time.Minute,
		lastRefresh:     time.Now(),
		proxies: []*proxyState{
			{key: "http://bad:80", url: mustParseURL(t, "http://bad:80")},
			{key: "http://blocked:80", url: mustParseURL(t, "http://blocked:80")},
			{key: "http://good:80", url: mustParseURL(t, "http://good:80")},
		},
	}

	transport := &RotatingProxyTransport{
		pool: pool,
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			proxyURL, _ := req.Context().Value(proxyContextKey{}).(*url.URL)
			switch proxyURL.Host {
			case "bad:80":
				return nil, errors.New("dial failed")
			case "blocked:80":
				return &http.Response{
					StatusCode: http.StatusForbidden,
					Body:       io.NopCloser(strings.NewReader("forbidden")),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			case "good:80":
				payload, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				if string(payload) != "hello" {
					t.Fatalf("expected request body to be replayed, got %q", string(payload))
				}

				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("ok")),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			default:
				return nil, errors.New("unexpected proxy")
			}
		}),
	}

	req, err := http.NewRequest(http.MethodPost, "https://example.com/path", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("http.NewRequest returned error: %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip returned error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 response, got %d", resp.StatusCode)
	}
	if !pool.proxies[2].unavailableUntil.IsZero() {
		t.Fatalf("expected successful proxy cooldown to be cleared")
	}
	if !pool.proxies[2].healthy {
		t.Fatalf("expected successful proxy to be marked healthy")
	}

	output := logs.String()
	if !strings.Contains(output, "proxy attempt target=https://example.com/path via=http://bad:80") {
		t.Fatalf("expected attempt log for bad proxy, got %q", output)
	}
	if !strings.Contains(output, "proxy failed target=https://example.com/path via=http://bad:80 err=dial failed") {
		t.Fatalf("expected failure log for bad proxy, got %q", output)
	}
	if !strings.Contains(output, "proxy retry status target=https://example.com/path via=http://blocked:80 status=403") {
		t.Fatalf("expected retry status log for blocked proxy, got %q", output)
	}
	if !strings.Contains(output, "proxy success target=https://example.com/path via=http://good:80 status=200") {
		t.Fatalf("expected success log for good proxy, got %q", output)
	}
}

func TestRotatingProxyTransportReturnsBadGatewayWhenPoolEmpty(t *testing.T) {
	transport := &RotatingProxyTransport{
		pool: &ProxyPool{lastRefresh: time.Now()},
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, errors.New("should not be called")
		}),
	}

	req, err := http.NewRequest(http.MethodGet, "https://example.com/path", nil)
	if err != nil {
		t.Fatalf("http.NewRequest returned error: %v", err)
	}

	_, err = transport.RoundTrip(req)
	if !errors.Is(err, errNoUpstreamProxy) {
		t.Fatalf("expected errNoUpstreamProxy, got %v", err)
	}
}

func TestProxyPoolEnsureFreshUsesTTLCache(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[{"proxy":"socks5://1.1.1.1:1080","protocol":"socks5","https":false}]`)
	}))
	defer server.Close()

	pool := NewProxyPool(log.New(io.Discard, "", 0), parseProxyProtocols("all"))
	pool.sourceURL = server.URL

	if err := pool.EnsureFresh(context.Background(), time.Now()); err != nil {
		t.Fatalf("EnsureFresh returned error: %v", err)
	}
	if requests != 1 {
		t.Fatalf("expected first EnsureFresh to fetch once, got %d", requests)
	}

	pool.mu.RLock()
	lastRefresh := pool.lastRefresh
	pool.mu.RUnlock()

	if err := pool.EnsureFresh(context.Background(), lastRefresh.Add(upstreamProxyListTTL-time.Second)); err != nil {
		t.Fatalf("EnsureFresh within TTL returned error: %v", err)
	}
	if requests != 1 {
		t.Fatalf("expected cached list within TTL, got %d fetches", requests)
	}

	if err := pool.EnsureFresh(context.Background(), lastRefresh.Add(upstreamProxyListTTL+time.Second)); err != nil {
		t.Fatalf("EnsureFresh after TTL returned error: %v", err)
	}
	if requests != 2 {
		t.Fatalf("expected refresh after TTL, got %d fetches", requests)
	}
}

func TestProxyPoolEnsureFreshKeepsStaleCacheOnRefreshFailure(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests > 1 {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[{"proxy":"socks5://1.1.1.1:1080","protocol":"socks5","https":false}]`)
	}))
	defer server.Close()

	pool := NewProxyPool(log.New(io.Discard, "", 0), parseProxyProtocols("all"))
	pool.sourceURL = server.URL

	if err := pool.EnsureFresh(context.Background(), time.Now()); err != nil {
		t.Fatalf("initial EnsureFresh returned error: %v", err)
	}

	pool.mu.RLock()
	lastRefresh := pool.lastRefresh
	pool.mu.RUnlock()

	if err := pool.EnsureFresh(context.Background(), lastRefresh.Add(upstreamProxyListTTL+time.Second)); err != nil {
		t.Fatalf("expected stale cache fallback, got error: %v", err)
	}
	if pool.Count() != 1 {
		t.Fatalf("expected cached proxies to remain available, got %d", pool.Count())
	}
}

func TestProxyPoolMaintainOnceChecksAndPromotesHealthyProxies(t *testing.T) {
	pool := NewProxyPool(log.New(io.Discard, "", 0), parseProxyProtocols("all"))
	pool.healthBatchSize = 8
	pool.healthWorkers = 1
	pool.healthTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		proxyURL, _ := req.Context().Value(proxyContextKey{}).(*url.URL)
		switch proxyURL.Host {
		case "healthy:80":
			return &http.Response{
				StatusCode: http.StatusNoContent,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		case "blocked:80":
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(strings.NewReader("forbidden")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		default:
			return nil, errors.New("unexpected proxy")
		}
	})
	pool.lastRefresh = time.Now()
	pool.proxies = []*proxyState{
		{key: "http://healthy:80", url: mustParseURL(t, "http://healthy:80")},
		{key: "http://blocked:80", url: mustParseURL(t, "http://blocked:80")},
	}

	now := time.Now()
	if err := pool.MaintainOnce(context.Background(), now); err != nil {
		t.Fatalf("MaintainOnce returned error: %v", err)
	}

	if !pool.proxies[0].healthy {
		t.Fatalf("expected healthy proxy to be promoted")
	}
	if !pool.proxies[0].verifiedAt.IsZero() {
		t.Fatalf("expected probe-only success to avoid verified state")
	}
	if pool.proxies[0].lastChecked.IsZero() {
		t.Fatalf("expected healthy proxy check timestamp to be recorded")
	}
	if pool.proxies[1].healthy {
		t.Fatalf("expected blocked proxy to remain unhealthy")
	}
	if pool.proxies[1].unavailableUntil.IsZero() {
		t.Fatalf("expected blocked proxy to enter cooldown")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func mustParseURL(t *testing.T, rawURL string) *url.URL {
	t.Helper()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("url.Parse(%q) returned error: %v", rawURL, err)
	}

	return parsed
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}

	return false
}
