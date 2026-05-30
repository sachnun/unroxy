package rewriter

import (
	"testing"
)

func TestToProxyURL(t *testing.T) {
	domain := "example.com"
	proxyBase := ""

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		// Special schemes - should not be rewritten
		{"data URI", "data:image/png;base64,abc", "data:image/png;base64,abc"},
		{"javascript", "javascript:void(0)", "javascript:void(0)"},
		{"mailto", "mailto:test@example.com", "mailto:test@example.com"},
		{"tel", "tel:+1234567890", "tel:+1234567890"},
		{"hash", "#section", "#section"},
		{"blob", "blob:http://example.com/123", "blob:http://example.com/123"},
		{"empty", "", ""},

		// Root-relative URLs
		{"root relative", "/path/to/file", "/example.com/path/to/file"},
		{"root relative with query", "/path?q=1", "/example.com/path?q=1"},
		{"root only", "/", "/example.com/"},

		// Protocol-relative URLs
		{"protocol relative", "//cdn.example.com/file.js", "/cdn.example.com/file.js"},
		{"protocol relative with path", "//other.com/path/file", "/other.com/path/file"},

		// Absolute URLs - same domain
		{"absolute same domain", "https://example.com/path", "/example.com/path"},
		{"absolute same domain with query", "https://example.com/path?q=1", "/example.com/path?q=1"},

		// Absolute URLs - different domain
		{"absolute different domain", "https://other.com/path", "/other.com/path"},

		// Relative URLs - should not be changed
		{"relative", "path/to/file", "path/to/file"},
		{"relative dot", "./path/to/file", "./path/to/file"},
		{"relative parent", "../path/to/file", "../path/to/file"},

		// Already proxied - should not double-proxy
		{"already proxied", "/example.com/path", "/example.com/path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ToProxyURL(tt.input, domain, proxyBase)
			if result != tt.expected {
				t.Errorf("ToProxyURL(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestToProxyURL_WithProxyBase(t *testing.T) {
	domain := "example.com"
	proxyBase := "/proxy"

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"root relative", "/path", "/proxy/example.com/path"},
		{"protocol relative", "//cdn.com/file", "/proxy/cdn.com/file"},
		{"absolute same domain", "https://example.com/path", "/proxy/example.com/path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ToProxyURL(tt.input, domain, proxyBase)
			if result != tt.expected {
				t.Errorf("ToProxyURL(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}


