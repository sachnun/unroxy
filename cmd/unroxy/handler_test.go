package main

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProxyHandler_parseRequest(t *testing.T) {
	h := NewProxyHandler()

	tests := []struct {
		name           string
		path           string
		query          string
		expectedDomain string
		expectedPath   string
		expectedQuery  string
	}{
		{
			name:           "simple domain and path",
			path:           "/example.com/path/to/file",
			query:          "",
			expectedDomain: "example.com",
			expectedPath:   "/path/to/file",
			expectedQuery:  "",
		},
		{
			name:           "domain only",
			path:           "/example.com",
			query:          "",
			expectedDomain: "example.com",
			expectedPath:   "/",
			expectedQuery:  "",
		},
		{
			name:           "domain with trailing slash",
			path:           "/example.com/",
			query:          "",
			expectedDomain: "example.com",
			expectedPath:   "/",
			expectedQuery:  "",
		},
		{
			name:           "with query string",
			path:           "/example.com/search",
			query:          "q=hello&page=1",
			expectedDomain: "example.com",
			expectedPath:   "/search",
			expectedQuery:  "q=hello&page=1",
		},
		{
			name:           "empty path",
			path:           "/",
			query:          "",
			expectedDomain: "",
			expectedPath:   "",
			expectedQuery:  "",
		},
		{
			name:           "subdomain",
			path:           "/api.example.com/v1/users",
			query:          "",
			expectedDomain: "api.example.com",
			expectedPath:   "/v1/users",
			expectedQuery:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://proxy.local"+tt.path+"?"+tt.query, nil)
			if tt.query == "" {
				req = httptest.NewRequest("GET", "http://proxy.local"+tt.path, nil)
			}

			domain, path, query := h.parseRequest(req)

			if domain != tt.expectedDomain {
				t.Errorf("domain: got %q, want %q", domain, tt.expectedDomain)
			}
			if path != tt.expectedPath {
				t.Errorf("path: got %q, want %q", path, tt.expectedPath)
			}
			if query != tt.expectedQuery {
				t.Errorf("query: got %q, want %q", query, tt.expectedQuery)
			}
		})
	}
}

func TestProxyHandler_ServeHTTP_InvalidPath(t *testing.T) {
	h := NewProxyHandler()
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", w.Code)
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
			wantCode: http.StatusInternalServerError, // transport is not *RotatingProxyTransport
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
			h := NewProxyHandlerWithTransport(mock)
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
	h := NewProxyHandlerWithTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
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
	h := NewProxyHandlerWithLoggerAndTransport(
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
	h := NewProxyHandler()
	req := httptest.NewRequest(http.MethodConnect, "http://proxy.local/example.com:443", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("Expected status 500, got %d", w.Code)
	}
}

func TestProxyHandler_RewriteProxy_Preserved(t *testing.T) {
	var gotReq *http.Request
	h := NewProxyHandlerWithLoggerAndTransport(
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
	h := NewProxyHandler()

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
	h := NewProxyHandlerWithLoggerAndTransport(logger, roundTripFunc(func(req *http.Request) (*http.Response, error) {
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
