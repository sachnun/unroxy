package core

import (
	"bufio"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newProxyHandlerWithTransport(logger *log.Logger, transport http.RoundTripper) *ProxyHandler {
	return &ProxyHandler{logger: logger, transport: transport}
}

func TestProxyHandler_ServeHTTP_InvalidPath(t *testing.T) {
	h := NewProxyHandler(nil, nil)
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}
	body := w.Body.String()
	if body == "" {
		t.Error("Expected non-empty response body")
	}
	if !strings.Contains(body, "Usage") {
		t.Error("Expected body to contain 'Usage'")
	}
}

func TestProxyHandler_ServeHTTP_RoutesCorrectly(t *testing.T) {
	mock := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	tests := []struct {
		name     string
		buildReq func() *http.Request
		wantCode int
	}{
		{
			name: "CONNECT routes to tunnel handler",
			buildReq: func() *http.Request {
				return httptest.NewRequest(http.MethodConnect, "http://proxy.local/example.com:443", nil)
			},
			wantCode: http.StatusInternalServerError,
		},
		{
			name: "absolute URI routes to forward proxy",
			buildReq: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "http://example.com/path", nil)
			},
			wantCode: http.StatusOK,
		},
		{
			name: "relative path serves index page",
			buildReq: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "/", nil)
			},
			wantCode: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newProxyHandlerWithTransport(nil, mock)
			req := tt.buildReq()
			w := httptest.NewRecorder()

			h.ServeHTTP(w, req)

			if w.Code != tt.wantCode {
				t.Errorf("Expected status %d, got %d", tt.wantCode, w.Code)
			}
		})
	}
}

func TestProxyHandler_ForwardProxy_UnsupportedScheme(t *testing.T) {
	h := newProxyHandlerWithTransport(nil, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
	}))

	req := httptest.NewRequest(http.MethodGet, "ftp://example.com/path", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400 for unsupported scheme, got %d", w.Code)
	}
}

func TestProxyHandler_ForwardProxy_ForwardsRequest(t *testing.T) {
	var gotReq *http.Request
	h := newProxyHandlerWithTransport(
		log.New(io.Discard, "", 0),
		roundTripFunc(func(req *http.Request) (*http.Response, error) {
			gotReq = req
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("forwarded")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/search?q=hello", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	if gotReq == nil {
		t.Fatal("Expected request to be forwarded")
	}
	if gotReq.URL.Host != "example.com" {
		t.Errorf("Expected host example.com, got %s", gotReq.URL.Host)
	}
	if gotReq.URL.Scheme != "http" {
		t.Errorf("Expected scheme http, got %s", gotReq.URL.Scheme)
	}
	if gotReq.URL.Path != "/search" {
		t.Errorf("Expected path /search, got %s", gotReq.URL.Path)
	}
}

func TestProxyHandler_ConnectTunnel(t *testing.T) {
	serverEnd, clientEnd := net.Pipe()
	defer clientEnd.Close()

	pool := NewProxyPool(nil, []*ProxyState{{
		Key: "test",
		URL: &url.URL{Scheme: "http", Host: "127.0.0.1:1"},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return serverEnd, nil
		},
	}})
	defaultTransport := NewRotatingProxyTransport(pool)
	router := NewPoolRouter(nil, defaultTransport)
	h := NewProxyHandler(log.New(io.Discard, "", 0), router)

	srv := httptest.NewServer(h)
	defer srv.Close()

	rawAddr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", rawAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := conn.Write([]byte("CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")); err != nil {
		t.Fatal(err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(clientEnd, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "ping" {
		t.Fatalf("expected ping, got %q", got)
	}

	if _, err := clientEnd.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	got2 := make([]byte, 4)
	if _, err := io.ReadFull(conn, got2); err != nil {
		t.Fatal(err)
	}
	if string(got2) != "pong" {
		t.Fatalf("expected pong, got %q", got2)
	}
}

func TestProxyHandler_ConnectTunnel_NoRotatingTransport(t *testing.T) {
	h := NewProxyHandler(nil, nil)
	req := httptest.NewRequest(http.MethodConnect, "http://proxy.local/example.com:443", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("Expected status 500, got %d", w.Code)
	}
}

func TestNewProxyHandler(t *testing.T) {
	h := NewProxyHandler(nil, nil)

	if h == nil {
		t.Error("Expected non-nil handler")
	}
	if h.transport != nil {
		t.Error("Expected nil transport by default")
	}
	if h.logger == nil {
		t.Error("Expected non-nil logger")
	}
}

func TestProxyHandlerDoesNotLogRequestDetails(t *testing.T) {
	var logs strings.Builder
	logger := log.New(&logs, "", 0)
	h := newProxyHandlerWithTransport(logger, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}))

	req := httptest.NewRequest(http.MethodGet, "http://proxy.local/example.com/search?q=hello", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	output := logs.String()
	if output != "" {
		t.Fatalf("expected no request detail log, got %q", output)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
}
