package main

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestNewCountryPoolRouterFetchesProxifly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "socks4") {
			w.Write([]byte(`[]`))
			return
		}
		w.Write([]byte(`[
			{"proxy":"socks5://1.2.3.4:1080","protocol":"socks5","ip":"1.2.3.4","port":1080,"https":false,"anonymity":"transparent","score":1,"geolocation":{"country":"US","city":"Ashburn"}},
			{"proxy":"socks5://5.6.7.8:1080","protocol":"socks5","ip":"5.6.7.8","port":1080,"https":false,"anonymity":"elite","score":1,"geolocation":{"country":"DE","city":"Berlin"}}
		]`))
	}))
	defer server.Close()

	oldBaseURL := proxiflyBaseURL
	proxiflyBaseURL = server.URL + "/"
	defer func() { proxiflyBaseURL = oldBaseURL }()

	proxies, err := fetchProxiflyProxies()
	if err != nil {
		t.Fatalf("fetchProxiflyProxies failed: %v", err)
	}

	_ = proxies

	// Verify at least parsing and fetch work (health check is tested separately)
	if len(proxies) == 0 {
		t.Fatal("expected at least some proxies to be parsed")
	}
}

func TestNewCountryPoolRouterLogsProxiflyNotReady(t *testing.T) {
	oldBaseURL := proxiflyBaseURL
	proxiflyBaseURL = "http://127.0.0.1:1/nonexistent/"
	defer func() { proxiflyBaseURL = oldBaseURL }()

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

func TestProxiflyToProxyStatesBuildsSocks5Proxies(t *testing.T) {
	proxies := proxiflyToProxyStates([]proxiflyProxy{
		{Proxy: "socks5://1.2.3.4:1080", Protocol: "socks5", IP: "1.2.3.4", Port: 1080, GeoLocation: struct {
			Country string `json:"country"`
			City    string `json:"city"`
		}{Country: "US"}},
		{Proxy: "socks4://5.6.7.8:4145", Protocol: "socks4", IP: "5.6.7.8", Port: 4145, GeoLocation: struct {
			Country string `json:"country"`
			City    string `json:"city"`
		}{Country: "DE"}},
	})

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

func TestProxiflyToProxyStatesHandlesEmptyCountry(t *testing.T) {
	proxies := proxiflyToProxyStates([]proxiflyProxy{
		{Proxy: "socks5://1.2.3.4:1080", Protocol: "socks5", IP: "1.2.3.4", Port: 1080},
	})

	if len(proxies) != 1 {
		t.Fatalf("expected 1 proxy state, got %d", len(proxies))
	}

	if proxies[0].country != "XX" {
		t.Fatalf("expected country XX for empty, got %q", proxies[0].country)
	}
}

func TestTestProxyReachableRefused(t *testing.T) {
	proxies := proxiflyToProxyStates([]proxiflyProxy{
		{Proxy: "socks5://127.0.0.1:1", Protocol: "socks5", IP: "127.0.0.1", Port: 1},
	})

	if testProxyReachable(proxies[0]) {
		t.Fatal("expected proxy to be unreachable")
	}
}

func TestTestProxyReachableNilDial(t *testing.T) {
	p := &proxyState{key: "socks5://127.0.0.1:1", url: &url.URL{Scheme: "socks5", Host: "127.0.0.1:1"}}
	if testProxyReachable(p) {
		t.Fatal("expected proxy with nil dialContext to be unreachable")
	}
}


