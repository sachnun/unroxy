package main

import (
	"bytes"
	"log"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNewUpstreamTransportUsesWebshareCredentials(t *testing.T) {
	t.Setenv(webshareUsernameEnv, "user")
	t.Setenv(websharePasswordEnv, "pass")

	var logs bytes.Buffer
	transport := newUpstreamTransport(log.New(&logs, "", 0))
	if transport == nil {
		t.Fatal("newUpstreamTransport() = nil, want non-nil transport")
	}

	output := logs.String()
	if !strings.Contains(output, "Proxy ready: p.webshare.io:80") {
		t.Fatalf("expected proxy mode log, got %q", output)
	}
	if strings.Contains(output, "user") || strings.Contains(output, "pass") {
		t.Fatalf("proxy credentials leaked in logs: %q", output)
	}
}

func TestNewUpstreamTransportLogsMissingWebshareCredentialsButStillReturnsTransport(t *testing.T) {
	t.Setenv(webshareUsernameEnv, "")
	t.Setenv(websharePasswordEnv, "")

	var logs bytes.Buffer
	transport := newUpstreamTransport(log.New(&logs, "", 0))
	if transport == nil {
		t.Fatal("newUpstreamTransport() = nil, want non-nil transport")
	}

	output := logs.String()
	if !strings.Contains(output, "Webshare proxy not ready") {
		t.Fatalf("expected credential failure log, got %q", output)
	}
	if !strings.Contains(output, "Proxy ready: p.webshare.io:80") {
		t.Fatalf("expected proxy mode log, got %q", output)
	}
}

func TestNewWebshareProxyPoolBuildsCredentialedSocks5Proxy(t *testing.T) {
	pool, err := NewWebshareProxyPool(log.New(&bytes.Buffer{}, "", 0), "user", "pass")
	if err != nil {
		t.Fatalf("NewWebshareProxyPool returned error: %v", err)
	}

	candidates := pool.Candidates(timeNow(), "example.com")
	if len(candidates) != 1 {
		t.Fatalf("expected 1 proxy candidate, got %d", len(candidates))
	}

	proxyURL := candidates[0].url
	if proxyURL.Scheme != "socks5" {
		t.Fatalf("expected socks5 proxy scheme, got %q", proxyURL.Scheme)
	}
	if proxyURL.Host != "p.webshare.io:80" {
		t.Fatalf("expected Webshare proxy host, got %q", proxyURL.Host)
	}
	username := proxyURL.User.Username()
	password, _ := proxyURL.User.Password()
	if username != "user" || password != "pass" {
		t.Fatalf("unexpected proxy credentials in URL: %s", (&url.URL{Scheme: proxyURL.Scheme, Host: proxyURL.Host}).String())
	}
	if candidates[0].key != "socks5://p.webshare.io:80" {
		t.Fatalf("expected credential-free proxy key, got %q", candidates[0].key)
	}
}

func timeNow() time.Time {
	return time.Now()
}
