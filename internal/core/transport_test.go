package core

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTransportRoutesRequestsThroughProxy(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Echo-Via", r.Header.Get("X-Via-Proxy"))
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()

	upstream := newUpstreamProxy()
	defer upstream.Close()
	upstream.addHeader = http.Header{"X-Via-Proxy": []string{"yes"}}

	transport := testTransport(upstream.proxyState(t))

	req, err := http.NewRequest(http.MethodGet, origin.URL+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Echo-Via"); got != "yes" {
		t.Fatalf("request did not go through proxy, Echo-Via = %q", got)
	}
}

func TestTransportRetriesRateLimitedProxyAndReplaysBody(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Echo-Via", r.Header.Get("X-Via-Proxy"))
		w.Write(body)
	}))
	defer origin.Close()

	blocked := newUpstreamProxy()
	defer blocked.Close()
	blocked.onRequest = func(*http.Request) (int, bool) { return http.StatusTooManyRequests, true }

	good := newUpstreamProxy()
	defer good.Close()
	good.addHeader = http.Header{"X-Via-Proxy": []string{"good"}}

	goodState := good.proxyState(t)
	goodState.Priority = 1
	pool := NewProxyPool(log.New(io.Discard, "", 0), []*ProxyState{
		blocked.proxyState(t),
		goodState,
	})
	pool.failedByHost = map[string]map[string]time.Time{
		"127.0.0.1": {goodState.Key: time.Now()},
	}
	transport := NewRotatingProxyTransport(pool)

	req, err := http.NewRequest(http.MethodPost, origin.URL+"/submit", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello" {
		t.Fatalf("final body = %q, want hello", body)
	}
	if got := resp.Header.Get("Echo-Via"); got != "good" {
		t.Fatalf("Echo-Via = %q, want good", got)
	}
	if !blocked.sawBody("hello") {
		t.Fatalf("rate limited proxy did not see body, got %v", blocked.bodies)
	}
	if !good.sawBody("hello") {
		t.Fatalf("good proxy did not see replayed body, got %v", good.bodies)
	}
}

func TestTransportFailsWhenAllProxiesRateLimited(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()

	first := newUpstreamProxy()
	defer first.Close()
	first.onRequest = func(*http.Request) (int, bool) { return http.StatusTooManyRequests, true }

	second := newUpstreamProxy()
	defer second.Close()
	second.onRequest = func(*http.Request) (int, bool) { return http.StatusTooManyRequests, true }

	transport := testTransport(first.proxyState(t), second.proxyState(t))

	req, err := http.NewRequest(http.MethodGet, origin.URL+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
		t.Fatalf("expected no response, got status %d", resp.StatusCode)
	}
	if err == nil {
		t.Fatal("expected error when all proxies are rate limited")
	}

	first.mu.Lock()
	second.mu.Lock()
	firstCalls, secondCalls := len(first.requests), len(second.requests)
	first.mu.Unlock()
	second.mu.Unlock()
	if firstCalls != 1 || secondCalls != 1 {
		t.Fatalf("calls = (%d, %d), want (1, 1)", firstCalls, secondCalls)
	}
}

func TestDialContextTunnelsThroughProxy(t *testing.T) {
	echoAddress := newEchoServer(t)

	upstream := newUpstreamProxy()
	defer upstream.Close()

	transport := testTransport(upstream.proxyState(t))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := transport.DialContext(ctx, "tcp", echoAddress)
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("echo = %q, want ping", got)
	}

	upstream.mu.Lock()
	connects := append([]string(nil), upstream.connects...)
	upstream.mu.Unlock()
	if len(connects) != 1 || connects[0] != echoAddress {
		t.Fatalf("tunnel targets = %v, want [%s]", connects, echoAddress)
	}
}

func TestTransportSetsEgressHeaders(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()

	upstream := newUpstreamProxy()
	defer upstream.Close()

	state := upstream.proxyState(t)
	state.IP = "203.0.113.9"
	state.ISP = "ExampleNet"

	transport := testTransport(state)

	req, err := http.NewRequest(http.MethodGet, origin.URL+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("x-unroxy-ip"); got != "203.0.113.9" {
		t.Fatalf("x-unroxy-ip = %q, want 203.0.113.9", got)
	}
	if got := resp.Header.Get("x-unroxy-isp"); got != "ExampleNet" {
		t.Fatalf("x-unroxy-isp = %q, want ExampleNet", got)
	}
}

func TestTransportWithoutProxiesFails(t *testing.T) {
	transport := testTransport()

	req, err := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	if _, err := transport.RoundTrip(req); err == nil {
		t.Fatal("expected error with empty pool")
	}
}
