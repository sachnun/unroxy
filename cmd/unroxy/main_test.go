package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewUpstreamTransportAlwaysEnabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[{"proxy":"socks5://1.1.1.1:1080","protocol":"socks5","https":false}]`)
	}))
	defer server.Close()

	originalURL := upstreamProxyListURL
	upstreamProxyListURL = server.URL
	defer func() {
		upstreamProxyListURL = originalURL
	}()

	var logs bytes.Buffer
	transport := newUpstreamTransport(log.New(&logs, "", 0))
	if transport == nil {
		t.Fatal("newUpstreamTransport() = nil, want non-nil transport")
	}

	output := logs.String()
	if !strings.Contains(output, "Loaded 1 fallback upstream proxies") {
		t.Fatalf("expected proxy preload log, got %q", output)
	}
	if !strings.Contains(output, "Upstream proxy fallback enabled with priority: socks5,https,http") {
		t.Fatalf("expected fallback mode log, got %q", output)
	}
	if strings.Contains(output, "PROXY") {
		t.Fatalf("unexpected legacy PROXY log output: %q", output)
	}
}

func TestNewUpstreamTransportLogsRefreshFailureButStillReturnsTransport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer server.Close()

	originalURL := upstreamProxyListURL
	upstreamProxyListURL = server.URL
	defer func() {
		upstreamProxyListURL = originalURL
	}()

	var logs bytes.Buffer
	transport := newUpstreamTransport(log.New(&logs, "", 0))
	if transport == nil {
		t.Fatal("newUpstreamTransport() = nil, want non-nil transport")
	}

	output := logs.String()
	if !strings.Contains(output, "Initial proxy list refresh failed, fallback proxy list unavailable") {
		t.Fatalf("expected refresh failure log, got %q", output)
	}
	if !strings.Contains(output, "Upstream proxy fallback enabled with priority: socks5,https,http") {
		t.Fatalf("expected fallback mode log, got %q", output)
	}
}
