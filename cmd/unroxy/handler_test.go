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

	tests := []struct {
		name string
		path string
	}{
		{"root only", "/"},
		{"empty", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://proxy.local"+tt.path, nil)
			w := httptest.NewRecorder()

			h.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("Expected status 400, got %d", w.Code)
			}
		})
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

func TestProxyHandlerLogsRequest(t *testing.T) {
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
	if !strings.Contains(output, "request method=GET") {
		t.Fatalf("expected request log, got %q", output)
	}
	if !strings.Contains(output, "source=/example.com/search?q=hello") {
		t.Fatalf("expected source path in log, got %q", output)
	}
	if !strings.Contains(output, "target=https://example.com/search?q=hello") {
		t.Fatalf("expected target URL in log, got %q", output)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
}
