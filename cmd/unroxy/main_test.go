package main

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/proxy"
)

func TestNewCountryPoolRouterFetchesProxifly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("socks5://1.2.3.4:1080,US,Ashburn\nsocks5://5.6.7.8:1080,DE,Berlin\n"))
	}))
	defer server.Close()

	oldURL := proxiflyCSVURL
	proxiflyCSVURL = server.URL
	defer func() { proxiflyCSVURL = oldURL }()

	proxies, err := fetchProxiflyProxies()
	if err != nil {
		t.Fatalf("fetchProxiflyProxies failed: %v", err)
	}

	if len(proxies) != 2 {
		t.Fatalf("expected 2 proxies, got %d", len(proxies))
	}
}

func TestNewCountryPoolRouterLogsProxiflyNotReady(t *testing.T) {
	oldURL := proxiflyCSVURL
	proxiflyCSVURL = "http://127.0.0.1:1/nonexistent/"
	defer func() { proxiflyCSVURL = oldURL }()

	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	router := newCountryPoolRouter(logger)
	if router == nil {
		t.Fatal("newCountryPoolRouter() = nil, want non-nil router")
	}

	output := logs.String()
	if !strings.Contains(output, "Proxifly proxy not ready") {
		t.Fatalf("expected failure log, got %q", output)
	}
}

func TestFetchProxiflyProxiesParsesCSV(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("socks5://1.2.3.4:1080,US,Ashburn\nsocks4://5.6.7.8:4145,DE,Berlin\n"))
	}))
	defer server.Close()

	oldURL := proxiflyCSVURL
	proxiflyCSVURL = server.URL
	defer func() { proxiflyCSVURL = oldURL }()

	proxies, err := fetchProxiflyProxies()
	if err != nil {
		t.Fatalf("fetchProxiflyProxies failed: %v", err)
	}

	if len(proxies) != 2 {
		t.Fatalf("expected 2 proxy states, got %d", len(proxies))
	}

	if proxies[0].url.Scheme != "socks5" {
		t.Fatalf("expected socks5 scheme, got %q", proxies[0].url.Scheme)
	}
	if proxies[0].url.Host != "1.2.3.4:1080" {
		t.Fatalf("expected host 1.2.3.4:1080, got %q", proxies[0].url.Host)
	}
	if proxies[0].country != "US" {
		t.Fatalf("expected country US, got %q", proxies[0].country)
	}
	if proxies[0].key != "socks5://1.2.3.4:1080" {
		t.Fatalf("expected key socks5://1.2.3.4:1080, got %q", proxies[0].key)
	}
	if proxies[0].dialContext == nil {
		t.Fatal("expected SOCKS5 dialContext to be non-nil")
	}

	if proxies[1].url.Scheme != "socks4" {
		t.Fatalf("expected socks4 scheme, got %q", proxies[1].url.Scheme)
	}
	if proxies[1].country != "DE" {
		t.Fatalf("expected country DE, got %q", proxies[1].country)
	}
	if proxies[1].dialContext == nil {
		t.Fatal("expected SOCKS4 dialContext to be non-nil")
	}
}

func TestFetchProxiflyProxiesHandlesEmptyCountry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("socks5://1.2.3.4:1080,,\n"))
	}))
	defer server.Close()

	oldURL := proxiflyCSVURL
	proxiflyCSVURL = server.URL
	defer func() { proxiflyCSVURL = oldURL }()

	proxies, err := fetchProxiflyProxies()
	if err != nil {
		t.Fatalf("fetchProxiflyProxies failed: %v", err)
	}

	if len(proxies) != 1 {
		t.Fatalf("expected 1 proxy state, got %d", len(proxies))
	}

	if proxies[0].country != "XX" {
		t.Fatalf("expected country XX for empty, got %q", proxies[0].country)
	}
}

func TestTestProxyReachableRefused(t *testing.T) {
	p := &proxyState{
		key: "socks5://127.0.0.1:1",
		url: &url.URL{Scheme: "socks5", Host: "127.0.0.1:1"},
	}
	d, err := proxy.FromURL(p.url, proxy.Direct)
	if err == nil {
		if d2, ok := d.(proxy.ContextDialer); ok {
			p.dialContext = d2.DialContext
		}
	}

	if testProxyReachable(p) {
		t.Fatal("expected proxy to be unreachable")
	}
}

func TestTestProxyReachableNilDial(t *testing.T) {
	p := &proxyState{key: "socks5://127.0.0.1:1", url: &url.URL{Scheme: "socks5", Host: "127.0.0.1:1"}}
	if testProxyReachable(p) {
		t.Fatal("expected proxy with nil dialContext to be unreachable")
	}
}
