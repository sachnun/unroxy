package proxifly

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"unroxy/internal/providers"
)

func TestFetchParsesCSV(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("socks5://1.2.3.4:1080,US,Ashburn\nsocks4://5.6.7.8:4145,DE,Berlin\n"))
	}))
	defer server.Close()

	oldURL := CSVURL
	CSVURL = server.URL
	defer func() { CSVURL = oldURL }()

	proxies, err := fetch()
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}

	if len(proxies) != 2 {
		t.Fatalf("expected 2 proxy states, got %d", len(proxies))
	}

	if proxies[0].URL.Scheme != "socks5" {
		t.Fatalf("expected socks5 scheme, got %q", proxies[0].URL.Scheme)
	}
	if proxies[0].URL.Host != "1.2.3.4:1080" {
		t.Fatalf("expected host 1.2.3.4:1080, got %q", proxies[0].URL.Host)
	}
	if proxies[0].Country != "US" {
		t.Fatalf("expected country US, got %q", proxies[0].Country)
	}
	if proxies[0].Key != "socks5://1.2.3.4:1080" {
		t.Fatalf("expected key socks5://1.2.3.4:1080, got %q", proxies[0].Key)
	}
	if proxies[0].DialContext == nil {
		t.Fatal("expected SOCKS5 DialContext to be non-nil")
	}

	if proxies[1].URL.Scheme != "socks4" {
		t.Fatalf("expected socks4 scheme, got %q", proxies[1].URL.Scheme)
	}
	if proxies[1].Country != "DE" {
		t.Fatalf("expected country DE, got %q", proxies[1].Country)
	}
	if proxies[1].DialContext == nil {
		t.Fatal("expected SOCKS4 DialContext to be non-nil")
	}
}

func TestFetchHandlesEmptyCountry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("socks5://1.2.3.4:1080,,\n"))
	}))
	defer server.Close()

	oldURL := CSVURL
	CSVURL = server.URL
	defer func() { CSVURL = oldURL }()

	proxies, err := fetch()
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	if len(proxies) != 1 {
		t.Fatalf("expected 1 proxy state, got %d", len(proxies))
	}
	if proxies[0].Country != "XX" {
		t.Fatalf("expected country XX for empty, got %q", proxies[0].Country)
	}
}

func TestStartLogsWhenNotReady(t *testing.T) {
	oldURL := CSVURL
	CSVURL = "http://127.0.0.1:1/nonexistent/"
	defer func() { CSVURL = oldURL }()

	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	host := providers.NewHost(logger)
	p := &Provider{}
	if err := p.Start(context.Background(), host, logger); err == nil {
		t.Fatal("expected error when Proxifly is unreachable")
	}

	if out := logs.String(); !strings.Contains(out, "Proxifly proxy not ready") {
		t.Fatalf("expected failure log, got %q", out)
	}
}
