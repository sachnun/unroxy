package core

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
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

func TestTransportUsesOneCandidatePerRequest(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()

	blocked := newUpstreamProxy()
	defer blocked.Close()
	blocked.onRequest = func(*http.Request) (int, bool) { return http.StatusTooManyRequests, true }

	good := newUpstreamProxy()
	defer good.Close()

	transport := testTransport(blocked.proxyState(t), good.proxyState(t))

	statuses := make([]int, 0, 2)
	for i := 0; i < 2; i++ {
		req, err := http.NewRequest(http.MethodGet, origin.URL+"/", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		resp.Body.Close()
		statuses = append(statuses, resp.StatusCode)
	}
	sort.Ints(statuses)
	if statuses[0] != http.StatusOK || statuses[1] != http.StatusTooManyRequests {
		t.Fatalf("statuses = %v, want [200 429]", statuses)
	}

	blocked.mu.Lock()
	blockedCalls := len(blocked.requests)
	blocked.mu.Unlock()
	good.mu.Lock()
	goodCalls := len(good.requests)
	good.mu.Unlock()
	if blockedCalls != 1 || goodCalls != 1 {
		t.Fatalf("proxy calls = (%d, %d), want (1, 1): a rate limited response must not fail over", blockedCalls, goodCalls)
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
