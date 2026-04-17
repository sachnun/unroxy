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

func TestParseProxyStates(t *testing.T) {
	body := []byte(`[
		{"proxy":"socks5://1.1.1.1:1080","protocol":"socks5","https":false},
		{"proxy":"socks://5.5.5.5:1080","protocol":"socks","https":false},
		{"proxy":"http://2.2.2.2:8080","protocol":"http","https":true},
		{"proxy":"http://3.3.3.3:8080","protocol":"http","https":false},
		{"proxy":"https://4.4.4.4:8443","protocol":"https","https":true},
		{"proxy":"socks5://1.1.1.1:1080","protocol":"socks5","https":false}
	]`)

	states, err := parseProxyStates(body)
	if err != nil {
		t.Fatalf("parseProxyStates returned error: %v", err)
	}

	if len(states) != 2 {
		t.Fatalf("expected 2 states, got %d", len(states))
	}

	if states[0].key != "socks5://1.1.1.1:1080" && states[1].key != "socks5://1.1.1.1:1080" {
		t.Fatalf("expected filtered list to keep socks5 proxy")
	}
	if states[0].key != "socks://5.5.5.5:1080" && states[1].key != "socks://5.5.5.5:1080" {
		t.Fatalf("expected filtered list to keep socks proxy")
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

	first := pool.Candidates(time.Now())
	if len(first) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(first))
	}
	if first[0].key != "http://1.1.1.1:80" || first[1].key != "http://2.2.2.2:80" || first[2].key != "http://3.3.3.3:80" {
		t.Fatalf("unexpected first candidate order: %q, %q, %q", first[0].key, first[1].key, first[2].key)
	}

	second := pool.Candidates(time.Now())
	if second[0].key != "http://2.2.2.2:80" || second[1].key != "http://3.3.3.3:80" || second[2].key != "http://1.1.1.1:80" {
		t.Fatalf("unexpected second candidate order: %q, %q, %q", second[0].key, second[1].key, second[2].key)
	}
}

func TestRotatingProxyTransportRetriesUntilSuccess(t *testing.T) {
	pool := &ProxyPool{
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

	pool := NewProxyPool(log.New(io.Discard, "", 0))
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

	pool := NewProxyPool(log.New(io.Discard, "", 0))
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
