package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNewCountryPoolRouterUsesWebshareAPIKeys(t *testing.T) {
	t.Setenv(webshareAPIKeyEnv, "api-key")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Token api-key" {
			t.Fatalf("unexpected authorization header %q", r.Header.Get("Authorization"))
		}

		switch r.URL.Path {
		case "/api/v2/subscription/plan/":
			io.WriteString(w, `{"results":[{"id":7,"status":"active","proxy_type":"free"},{"id":8,"status":"active","proxy_type":"shared"}]}`)
		case "/api/v2/proxy/list/":
			if r.URL.Query().Get("mode") != "direct" || r.URL.Query().Get("plan_id") != "7" {
				t.Fatalf("unexpected proxy list query %q", r.URL.RawQuery)
			}
			io.WriteString(w, `{"next":null,"results":[{"id":"d-1","username":"user","password":"pass","proxy_address":"1.2.3.4","port":8080,"country_code":"US"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	oldPlansURL := websharePlansURL
	oldProxyListURL := webshareProxyListURL
	oldClient := webshareAPIHTTPClient
	websharePlansURL = server.URL + "/api/v2/subscription/plan/"
	webshareProxyListURL = server.URL + "/api/v2/proxy/list/"
	webshareAPIHTTPClient = server.Client()
	defer func() {
		websharePlansURL = oldPlansURL
		webshareProxyListURL = oldProxyListURL
		webshareAPIHTTPClient = oldClient
	}()

	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	router := newCountryPoolRouter(logger)
	if router == nil {
		t.Fatal("newCountryPoolRouter() = nil, want non-nil router")
	}

	output := logs.String()
	if !strings.Contains(output, "Pool \"US\" ready: 1 proxies") {
		t.Fatalf("expected country pool log, got %q", output)
	}
	if strings.Contains(output, "api-key") || strings.Contains(output, "user") || strings.Contains(output, "pass") {
		t.Fatalf("Webshare secret leaked in logs: %q", output)
	}
}

func TestNewCountryPoolRouterLogsMissingWebshareAPIKey(t *testing.T) {
	t.Setenv(webshareAPIKeyEnv, "")

	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	router := newCountryPoolRouter(logger)
	if router == nil {
		t.Fatal("newCountryPoolRouter() = nil, want non-nil router")
	}

	output := logs.String()
	if !strings.Contains(output, "Webshare proxy not ready") {
		t.Fatalf("expected API key failure log, got %q", output)
	}
}

func TestParseWebshareAPIKeys(t *testing.T) {
	apiKeys, err := parseWebshareAPIKeys(" first , second, ")
	if err != nil {
		t.Fatalf("parseWebshareAPIKeys returned error: %v", err)
	}

	want := []string{"first", "second"}
	if len(apiKeys) != len(want) {
		t.Fatalf("expected %d API keys, got %d", len(want), len(apiKeys))
	}
	for i := range want {
		if apiKeys[i] != want[i] {
			t.Fatalf("API keys = %q, want %q", apiKeys, want)
		}
	}
}

func TestWebshareProxyStatesFromAPIBuildsCredentialedSocks5Proxies(t *testing.T) {
	proxies, err := webshareProxyStatesFromAPI([]webshareProxy{
		{ID: "d-1", ProxyAddress: "1.2.3.4", Port: 8080, Username: "user", Password: "pass"},
		{ID: "d-2", ProxyAddress: "5.6.7.8", Port: 9090, Username: "user2", Password: "pass2"},
	}, 0)
	if err != nil {
		t.Fatalf("webshareProxyStatesFromAPI returned error: %v", err)
	}

	pool := NewProxyPool(log.New(&bytes.Buffer{}, "", 0), proxies)
	candidates := pool.Candidates(timeNow(), "example.com")
	if len(candidates) != 2 {
		t.Fatalf("expected 2 proxy candidates, got %d", len(candidates))
	}

	proxyURL := candidates[0].url
	if proxyURL.Scheme != "socks5" {
		t.Fatalf("expected socks5 proxy scheme, got %q", proxyURL.Scheme)
	}
	if proxyURL.Host != "1.2.3.4:8080" {
		t.Fatalf("expected Webshare proxy host, got %q", proxyURL.Host)
	}
	username := proxyURL.User.Username()
	password, _ := proxyURL.User.Password()
	if username != "user" || password != "pass" {
		t.Fatalf("unexpected proxy credentials in URL: %s", (&url.URL{Scheme: proxyURL.Scheme, Host: proxyURL.Host}).String())
	}
	if candidates[0].key != "socks5://1.2.3.4:8080#d-1" {
		t.Fatalf("expected credential-free proxy key, got %q", candidates[0].key)
	}
}

func TestFetchWebshareDirectProxyStatesUsesAPIURLAndAuthorization(t *testing.T) {
	var requestedQueries []string
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Token api-key" {
			t.Fatalf("unexpected authorization header %q", r.Header.Get("Authorization"))
		}
		requestedQueries = append(requestedQueries, r.URL.RawQuery)

		switch r.URL.Query().Get("page") {
		case "1":
			io.WriteString(w, `{"next":"`+serverURL+`/api/v2/proxy/list/?mode=direct&page=2&page_size=100&plan_id=7","results":[{"id":"d-1","username":"user","password":"pass","proxy_address":"1.2.3.4","port":8080}]}`)
		case "2":
			io.WriteString(w, `{"next":null,"results":[{"id":"d-2","username":"user2","password":"pass2","proxy_address":"5.6.7.8","port":9090}]}`)
		default:
			t.Fatalf("unexpected page %q", r.URL.Query().Get("page"))
		}
	}))
	serverURL = server.URL
	defer server.Close()

	oldProxyListURL := webshareProxyListURL
	webshareProxyListURL = server.URL + "/api/v2/proxy/list/"
	defer func() { webshareProxyListURL = oldProxyListURL }()

	proxies, err := fetchWebshareDirectProxyStates(server.Client(), "api-key", 7, 2)
	if err != nil {
		t.Fatalf("fetchWebshareDirectProxyStates returned error: %v", err)
	}
	if len(proxies) != 2 {
		t.Fatalf("expected 2 proxies, got %d", len(proxies))
	}
	if len(requestedQueries) != 2 || !strings.Contains(requestedQueries[0], "mode=direct") || !strings.Contains(requestedQueries[0], "plan_id=7") {
		t.Fatalf("unexpected proxy list queries %q", requestedQueries)
	}
	if proxies[0].key != "socks5://1.2.3.4:8080#d-1" {
		t.Fatalf("unexpected proxy key %q", proxies[0].key)
	}
}

func timeNow() time.Time {
	return time.Now()
}
