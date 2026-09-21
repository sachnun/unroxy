package core

import (
	"bufio"
	"encoding/base64"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestForwardProxyDeliversRequestToOrigin(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Origin-Method", r.Method)
		w.Header().Set("X-Origin-Path", r.URL.Path)
		w.Header().Set("X-Origin-Query", r.URL.RawQuery)
		body, _ := io.ReadAll(r.Body)
		w.Write(body)
	}))
	defer origin.Close()

	upstream := newUpstreamProxy()
	defer upstream.Close()

	handler := testHandler(t, testTransport(upstream.proxyState(t)))
	client := proxyClient(t, handler)

	resp, err := client.Post(origin.URL+"/search?q=hello", "text/plain", strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("request through proxy: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "payload" {
		t.Fatalf("body = %q, want %q", body, "payload")
	}
	if got := resp.Header.Get("X-Origin-Method"); got != http.MethodPost {
		t.Fatalf("origin method = %q, want POST", got)
	}
	if got := resp.Header.Get("X-Origin-Path"); got != "/search" {
		t.Fatalf("origin path = %q, want /search", got)
	}
	if got := resp.Header.Get("X-Origin-Query"); got != "q=hello" {
		t.Fatalf("origin query = %q, want q=hello", got)
	}
	if !upstream.sawBody("payload") {
		t.Fatalf("upstream proxy did not receive body, got %v", upstream.bodies)
	}
}

func TestForwardProxyStripsSpoofedClientHeaders(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range []string{"X-Real-IP", "X-Forwarded-Host", "X-Forwarded-Proto", "CF-Connecting-IP"} {
			if v := r.Header.Get(h); v != "" {
				w.Header().Set("Leaked-"+h, v)
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()

	upstream := newUpstreamProxy()
	defer upstream.Close()

	handler := testHandler(t, testTransport(upstream.proxyState(t)))
	client := proxyClient(t, handler)

	req, err := http.NewRequest(http.MethodGet, origin.URL+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Real-IP", "9.9.9.9")
	req.Header.Set("X-Forwarded-Host", "evil.example")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("CF-Connecting-IP", "9.9.9.9")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request through proxy: %v", err)
	}
	defer resp.Body.Close()

	for _, h := range []string{"Leaked-X-Real-IP", "Leaked-X-Forwarded-Host", "Leaked-X-Forwarded-Proto", "Leaked-CF-Connecting-IP"} {
		if v := resp.Header.Get(h); v != "" {
			t.Fatalf("spoofed header %s reached origin: %q", h, v)
		}
	}
}

func TestForwardProxyRejectsUnsupportedScheme(t *testing.T) {
	handler := NewProxyHandler(log.New(io.Discard, "", 0), nil)

	req := httptest.NewRequest(http.MethodGet, "ftp://example.com/file", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestConnectTunnelThroughUpstreamProxy(t *testing.T) {
	echoAddress := newEchoServer(t)

	upstream := newUpstreamProxy()
	defer upstream.Close()

	handler := testHandler(t, testTransport(upstream.proxyState(t)))

	conn, err := netDial(handler.URL)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := conn.Write([]byte("CONNECT " + echoAddress + " HTTP/1.1\r\nHost: " + echoAddress + "\r\n\r\n")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write tunnel payload: %v", err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(reader, got); err != nil {
		t.Fatalf("read tunnel echo: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("tunnel echo = %q, want ping", got)
	}

	upstream.mu.Lock()
	connects := append([]string(nil), upstream.connects...)
	upstream.mu.Unlock()
	if len(connects) != 1 || connects[0] != echoAddress {
		t.Fatalf("upstream CONNECT targets = %v, want [%s]", connects, echoAddress)
	}
}

func TestConnectTunnelWithoutTransportFails(t *testing.T) {
	req := httptest.NewRequest(http.MethodConnect, "http://proxy.local/example.com:443", nil)
	w := httptest.NewRecorder()

	NewProxyHandler(log.New(io.Discard, "", 0), nil).ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func TestRegionRoutingSelectsMatchingUpstream(t *testing.T) {
	usProxy := newUpstreamProxy()
	defer usProxy.Close()
	usProxy.addHeader = http.Header{"X-Upstream-Region": []string{"us"}}

	deProxy := newUpstreamProxy()
	defer deProxy.Close()
	deProxy.addHeader = http.Header{"X-Upstream-Region": []string{"de"}}

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Echo-Region", r.Header.Get("X-Upstream-Region"))
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()

	router := NewPoolRouter([]*NamedPool{
		{Name: "US", Username: "us", Pool: NewProxyPool(log.New(io.Discard, "", 0), []*ProxyState{usProxy.proxyState(t)}), Transport: testTransport(usProxy.proxyState(t))},
		{Name: "DE", Username: "de", Pool: NewProxyPool(log.New(io.Discard, "", 0), []*ProxyState{deProxy.proxyState(t)}), Transport: testTransport(deProxy.proxyState(t))},
	}, testTransport(usProxy.proxyState(t)))

	handler := httptest.NewServer(NewProxyHandler(log.New(io.Discard, "", 0), router))
	defer handler.Close()

	for username, want := range map[string]string{"us": "us", "de": "de", "US": "us"} {
		req, err := http.NewRequest(http.MethodGet, origin.URL+"/", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(username+":")))

		resp, err := proxyClient(t, handler).Do(req)
		if err != nil {
			t.Fatalf("request as %q: %v", username, err)
		}
		region := resp.Header.Get("Echo-Region")
		resp.Body.Close()
		if region != want {
			t.Fatalf("as %q: origin saw region %q, want %q", username, region, want)
		}
	}
}

func TestRegionRoutingUnknownUserFails(t *testing.T) {
	router := NewPoolRouter([]*NamedPool{
		{Name: "US", Username: "us", Transport: testTransport()},
	}, testTransport())

	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("nope:")))
	w := httptest.NewRecorder()

	NewProxyHandler(log.New(io.Discard, "", 0), router).ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

func TestIndexPageListsPools(t *testing.T) {
	router := NewPoolRouter([]*NamedPool{
		{Name: "US", Username: "us", Pool: NewProxyPool(log.New(io.Discard, "", 0), []*ProxyState{
			proxyStateURL(t, "a", "http://1.1.1.1:80"),
			proxyStateURL(t, "b", "http://2.2.2.2:80"),
		})},
		{Name: "DE", Username: "de", Pool: NewProxyPool(log.New(io.Discard, "", 0), []*ProxyState{
			proxyStateURL(t, "c", "http://3.3.3.3:80"),
		})},
	}, testTransport())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	NewProxyHandler(log.New(io.Discard, "", 0), router).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"Usage", "US(2)", "DE(1)", "Total: 3 proxies"} {
		if !strings.Contains(body, want) {
			t.Fatalf("index page missing %q:\n%s", want, body)
		}
	}
}
