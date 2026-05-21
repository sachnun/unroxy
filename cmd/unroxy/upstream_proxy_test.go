package main

import (
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

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

func TestProxyPoolCandidatesRoundRobinPerHost(t *testing.T) {
	pool := &ProxyPool{
		proxies: []*proxyState{
			{key: "http://1.1.1.1:80", url: mustParseURL(t, "http://1.1.1.1:80")},
			{key: "http://2.2.2.2:80", url: mustParseURL(t, "http://2.2.2.2:80")},
			{key: "http://3.3.3.3:80", url: mustParseURL(t, "http://3.3.3.3:80")},
		},
	}

	firstIPWho := pool.Candidates(time.Now(), "ipwho.is")
	firstFavicon := pool.Candidates(time.Now(), "favicon.ico")
	secondIPWho := pool.Candidates(time.Now(), "ipwho.is")

	if firstIPWho[0].key != "http://1.1.1.1:80" {
		t.Fatalf("unexpected first ipwho.is proxy %q", firstIPWho[0].key)
	}
	if firstFavicon[0].key != "http://1.1.1.1:80" {
		t.Fatalf("unexpected first favicon proxy %q", firstFavicon[0].key)
	}
	if secondIPWho[0].key != "http://2.2.2.2:80" {
		t.Fatalf("unexpected second ipwho.is proxy %q", secondIPWho[0].key)
	}
}

func TestProxyPoolCandidatesFailedForHostLast(t *testing.T) {
	now := time.Now()
	pool := &ProxyPool{
		proxies: []*proxyState{
			{key: "http://failed:80", url: mustParseURL(t, "http://failed:80")},
			{key: "http://verified:80", url: mustParseURL(t, "http://verified:80"), healthy: true, lastChecked: now.Add(-time.Minute), verifiedAt: now.Add(-30 * time.Second)},
			{key: "http://untested:80", url: mustParseURL(t, "http://untested:80")},
		},
		failedByHost: map[string]map[string]bool{
			"ipwho.is": {"http://failed:80": true},
		},
	}

	candidates := pool.Candidates(now, "ipwho.is")
	got := []string{candidates[0].key, candidates[1].key, candidates[2].key}
	want := []string{"http://verified:80", "http://untested:80", "http://failed:80"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate order = %v, want %v", got, want)
		}
	}
}

func TestProxyPoolCandidatesPreserveRoundRobinOverProtocolPriority(t *testing.T) {
	now := time.Now()
	pool := &ProxyPool{
		proxies: []*proxyState{
			{key: "http://http:80", url: mustParseURL(t, "http://http:80"), healthy: true, lastChecked: now.Add(-time.Minute)},
			{key: "https://https:443", url: mustParseURL(t, "https://https:443"), healthy: true, lastChecked: now.Add(-time.Minute)},
			{key: "socks5://socks:1080", url: mustParseURL(t, "socks5://socks:1080"), healthy: true, lastChecked: now.Add(-time.Minute)},
		},
	}

	candidates := pool.Candidates(now, "")
	got := []string{candidates[0].key, candidates[1].key, candidates[2].key}
	want := []string{"http://http:80", "https://https:443", "socks5://socks:1080"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate order = %v, want %v", got, want)
		}
	}
}

func TestRotatingProxyTransportUsesProxyForEveryRequest(t *testing.T) {
	pool := &ProxyPool{
		proxies: []*proxyState{
			{key: "socks5://good:1080", url: mustParseURL(t, "socks5://good:1080")},
		},
	}

	directCalls := 0
	proxyCalls := 0
	transport := &RotatingProxyTransport{
		pool: pool,
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			proxyURL, _ := req.Context().Value(proxyContextKey{}).(*url.URL)
			if proxyURL == nil {
				directCalls++
				return nil, errors.New("direct should not be called")
			}

			proxyCalls++
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("proxy")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
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
	if directCalls != 0 {
		t.Fatalf("expected 0 direct calls, got %d", directCalls)
	}
	if proxyCalls != 1 {
		t.Fatalf("expected 1 proxy call, got %d", proxyCalls)
	}
}

func TestProxyPoolCandidatesRotateEvenForVerifiedHost(t *testing.T) {
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
	want := []string{"http://global:80", "http://host:80", "http://probed:80"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate order = %v, want %v", got, want)
		}
	}
}

func TestProxyPoolReplaceSwapsProxiesAndPreservesRotationBounds(t *testing.T) {
	now := time.Now()
	pool := &ProxyPool{
		next: 5,
		proxies: []*proxyState{
			{key: "http://old:80", url: mustParseURL(t, "http://old:80")},
		},
	}

	pool.Replace([]*proxyState{
		{key: "http://new1:80", url: mustParseURL(t, "http://new1:80")},
		{key: "http://new2:80", url: mustParseURL(t, "http://new2:80")},
	})

	candidates := pool.Candidates(now, "")
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
	if candidates[0].key != "http://new2:80" || candidates[1].key != "http://new1:80" {
		t.Fatalf("unexpected candidate order after replace: %q, %q", candidates[0].key, candidates[1].key)
	}
}

func TestRotatingProxyTransportRetriesProxyCandidatesOnRateLimit(t *testing.T) {
	var logs strings.Builder
	pool := &ProxyPool{
		logger: log.New(&logs, "", 0),
		proxies: []*proxyState{
			{key: "https://blocked:443", url: mustParseURL(t, "https://blocked:443")},
			{key: "http://good:80", url: mustParseURL(t, "http://good:80")},
		},
	}

	directCalls := 0
	transport := &RotatingProxyTransport{
		pool: pool,
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			proxyURL, _ := req.Context().Value(proxyContextKey{}).(*url.URL)
			if proxyURL == nil {
				directCalls++
				return nil, errors.New("direct should not be called")
			}

			switch proxyURL.Host {
			case "blocked:443":
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader("rate limited")),
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
	if directCalls != 0 {
		t.Fatalf("expected 0 direct calls, got %d", directCalls)
	}
	if !pool.proxies[1].healthy {
		t.Fatalf("expected successful proxy to be marked healthy")
	}

	output := logs.String()
	if !strings.Contains(output, "[RETRY] example.com/path -> blocked (429)") {
		t.Fatalf("expected retry status log for blocked proxy, got %q", output)
	}
	if !strings.Contains(output, "[OK] example.com/path -> good (200)") {
		t.Fatalf("expected success log for good proxy, got %q", output)
	}
}

func TestRequestTargetLogIncludesPathAndQuery(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://ipwho.is/gome?x=xxx", nil)
	if err != nil {
		t.Fatalf("http.NewRequest returned error: %v", err)
	}

	if got := requestTargetLog(req); got != "ipwho.is/gome?x=xxx" {
		t.Fatalf("requestTargetLog() = %q, want %q", got, "ipwho.is/gome?x=xxx")
	}
}

func TestRotatingProxyTransportDoesNotRetryProxyError(t *testing.T) {
	pool := &ProxyPool{
		proxies: []*proxyState{
			{key: "socks5://bad:1080", url: mustParseURL(t, "socks5://bad:1080")},
			{key: "http://good:80", url: mustParseURL(t, "http://good:80")},
		},
	}

	proxyCalls := 0
	transport := &RotatingProxyTransport{
		pool: pool,
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			proxyURL, _ := req.Context().Value(proxyContextKey{}).(*url.URL)
			if proxyURL == nil {
				return nil, errors.New("direct should not be called")
			}

			proxyCalls++
			if proxyURL.Host != "bad:1080" {
				return nil, errors.New("unexpected retry")
			}

			return nil, errors.New("dial failed")
		}),
	}

	req, err := http.NewRequest(http.MethodGet, "https://example.com/path", nil)
	if err != nil {
		t.Fatalf("http.NewRequest returned error: %v", err)
	}

	_, err = transport.RoundTrip(req)
	if err == nil {
		t.Fatal("expected RoundTrip error")
	}
	if !strings.Contains(err.Error(), "dial failed") {
		t.Fatalf("expected proxy failure error, got %v", err)
	}
	if proxyCalls != 1 {
		t.Fatalf("expected 1 proxy call, got %d", proxyCalls)
	}
	failed := pool.failedByHost["example.com"]["socks5://bad:1080"]
	if !failed {
		t.Fatalf("expected proxy error to move proxy behind healthy candidates")
	}
}

func TestRotatingProxyTransportReturnsErrorWhenAllProxiesFail(t *testing.T) {
	transport := &RotatingProxyTransport{
		pool: &ProxyPool{
			proxies: []*proxyState{
				{key: "socks5://bad:1080", url: mustParseURL(t, "socks5://bad:1080")},
			},
		},
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			proxyURL, _ := req.Context().Value(proxyContextKey{}).(*url.URL)
			if proxyURL != nil {
				return nil, errors.New("proxy failed")
			}

			return nil, errors.New("direct should not be called")
		}),
	}

	req, err := http.NewRequest(http.MethodGet, "https://example.com/path", nil)
	if err != nil {
		t.Fatalf("http.NewRequest returned error: %v", err)
	}

	_, err = transport.RoundTrip(req)
	if err == nil {
		t.Fatal("expected RoundTrip error")
	}
	if !strings.Contains(err.Error(), "proxy failed") {
		t.Fatalf("expected proxy failure error, got %v", err)
	}
}

func TestRotatingProxyTransportUsesProxyForRepeatedRequests(t *testing.T) {
	pool := &ProxyPool{
		proxies: []*proxyState{
			{key: "socks5://good:1080", url: mustParseURL(t, "socks5://good:1080")},
		},
	}

	directCalls := 0
	proxyCalls := 0
	transport := &RotatingProxyTransport{
		pool: pool,
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			proxyURL, _ := req.Context().Value(proxyContextKey{}).(*url.URL)
			if proxyURL == nil {
				directCalls++
				return nil, errors.New("direct should not be called")
			}

			proxyCalls++
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("proxy ok")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
	}

	req1, err := http.NewRequest(http.MethodGet, "https://example.com/path", nil)
	if err != nil {
		t.Fatalf("http.NewRequest returned error: %v", err)
	}
	resp1, err := transport.RoundTrip(req1)
	if err != nil {
		t.Fatalf("first RoundTrip returned error: %v", err)
	}
	resp1.Body.Close()

	req2, err := http.NewRequest(http.MethodGet, "https://example.com/again", nil)
	if err != nil {
		t.Fatalf("http.NewRequest returned error: %v", err)
	}
	resp2, err := transport.RoundTrip(req2)
	if err != nil {
		t.Fatalf("second RoundTrip returned error: %v", err)
	}
	defer resp2.Body.Close()

	if directCalls != 0 {
		t.Fatalf("expected 0 direct calls, got %d", directCalls)
	}
	if proxyCalls != 2 {
		t.Fatalf("expected 2 proxy calls, got %d", proxyCalls)
	}
}

func TestNewProxyAwareTransportDisablesConnectionReuse(t *testing.T) {
	transport, ok := newProxyAwareTransport().(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport")
	}

	if !transport.DisableKeepAlives {
		t.Fatalf("expected DisableKeepAlives to prevent proxy rotation bypass")
	}
	if transport.ForceAttemptHTTP2 {
		t.Fatalf("expected ForceAttemptHTTP2 disabled with per-request proxy rotation")
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
