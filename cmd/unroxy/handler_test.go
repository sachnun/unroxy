package main

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
	if !strings.Contains(body, "Rewrite") {
		t.Error("Expected body to contain 'Rewrite'")
	}
}

func TestIsValidDomainAcceptsPrivatePublicSuffix(t *testing.T) {
	if !isValidDomain("httpbin.org") {
		t.Fatal("expected httpbin.org to be valid")
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
			name: "relative path routes to rewrite proxy",
			buildReq: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "/example.com/path", nil)
			},
			wantCode: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewProxyHandler(nil, mock)
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
	h := NewProxyHandler(nil, roundTripFunc(func(req *http.Request) (*http.Response, error) {
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
	h := NewProxyHandler(
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

func TestProxyHandler_ConnectTunnel_NoRotatingTransport(t *testing.T) {
	h := NewProxyHandler(nil, nil)
	req := httptest.NewRequest(http.MethodConnect, "http://proxy.local/example.com:443", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("Expected status 500, got %d", w.Code)
	}
}

func TestProxyHandler_RewriteProxy_Preserved(t *testing.T) {
	var gotReq *http.Request
	h := NewProxyHandler(
		log.New(io.Discard, "", 0),
		roundTripFunc(func(req *http.Request) (*http.Response, error) {
			gotReq = req
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("rewritten")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/example.com/path", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	if gotReq == nil {
		t.Fatal("Expected request to be proxied")
	}
	if gotReq.URL.Host != "example.com" {
		t.Errorf("Expected host example.com, got %s", gotReq.URL.Host)
	}
	if gotReq.URL.Scheme != "https" {
		t.Errorf("Expected scheme https, got %s", gotReq.URL.Scheme)
	}
	if gotReq.URL.Path != "/path" {
		t.Errorf("Expected path /path, got %s", gotReq.URL.Path)
	}
}

func TestNewProxyHandler(t *testing.T) {
	h := NewProxyHandler(nil, nil)

	if h == nil {
		t.Error("Expected non-nil handler")
	}
	if h.htmlRewriter == nil {
		t.Error("Expected non-nil HTML rewriter")
	}
	if h.cssRewriter == nil {
		t.Error("Expected non-nil CSS rewriter")
	}
	if h.jsRewriter == nil {
		t.Error("Expected non-nil JS rewriter")
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
	h := NewProxyHandler(logger, roundTripFunc(func(req *http.Request) (*http.Response, error) {
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

func TestParsePoolRequest_WarpSkippingInvalidDomain(t *testing.T) {
	router := NewPoolRouter([]*NamedPool{
		{Name: "WARP", Username: "WARP"},
		{Name: "ID", Username: "ID"},
	}, nil)
	h := &ProxyHandler{router: router}

	req := httptest.NewRequest(http.MethodGet, "/warp/id/ipwho.is", nil)
	pool, domain, path, _ := h.parsePoolRequest(req)

	if pool != "WARP" {
		t.Errorf("expected pool=WARP, got %s", pool)
	}
	if domain != "ipwho.is" {
		t.Errorf("expected domain=ipwho.is, got %s", domain)
	}
	if path != "/" {
		t.Errorf("expected path=/, got %s", path)
	}
}

func TestParsePoolRequest_WarpWithValidDomain(t *testing.T) {
	router := NewPoolRouter([]*NamedPool{
		{Name: "WARP", Username: "WARP"},
	}, nil)
	h := &ProxyHandler{router: router}

	req := httptest.NewRequest(http.MethodGet, "/warp/example.com/path", nil)
	pool, domain, path, _ := h.parsePoolRequest(req)

	if pool != "WARP" {
		t.Errorf("expected pool=WARP, got %s", pool)
	}
	if domain != "example.com" {
		t.Errorf("expected domain=example.com, got %s", domain)
	}
	if path != "/path" {
		t.Errorf("expected path=/path, got %s", path)
	}
}

func TestParsePoolRequest_WarpCompoundKey(t *testing.T) {
	router := NewPoolRouter([]*NamedPool{
		{Name: "WARP", Username: "WARP"},
		{Name: "WARP/US", Username: "WARP/US"},
	}, nil)
	h := &ProxyHandler{router: router}

	req := httptest.NewRequest(http.MethodGet, "/warp/us/example.com", nil)
	pool, domain, path, _ := h.parsePoolRequest(req)

	if pool != "WARP/US" {
		t.Errorf("expected pool=WARP/US, got %s", pool)
	}
	if domain != "example.com" {
		t.Errorf("expected domain=example.com, got %s", domain)
	}
	if path != "/" {
		t.Errorf("expected path=/, got %s", path)
	}
}

func TestParsePoolRequest_CountryPool(t *testing.T) {
	router := NewPoolRouter([]*NamedPool{
		{Name: "ID", Username: "ID"},
	}, nil)
	h := &ProxyHandler{router: router}

	req := httptest.NewRequest(http.MethodGet, "/id/ipwho.is", nil)
	pool, domain, path, _ := h.parsePoolRequest(req)

	if pool != "ID" {
		t.Errorf("expected pool=ID, got %s", pool)
	}
	if domain != "ipwho.is" {
		t.Errorf("expected domain=ipwho.is, got %s", domain)
	}
	if path != "/" {
		t.Errorf("expected path=/, got %s", path)
	}
}
