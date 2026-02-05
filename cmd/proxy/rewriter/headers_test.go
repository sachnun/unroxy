package rewriter

import (
	"net/http"
	"testing"
)

func TestRewriteHeaders_Location(t *testing.T) {
	domain := "example.com"
	proxyBase := ""

	tests := []struct {
		name     string
		location string
		expected string
	}{
		{"root relative", "/new-page", "/example.com/new-page"},
		{"absolute same domain", "https://example.com/page", "/example.com/page"},
		{"protocol relative", "//cdn.example.com/file", "/cdn.example.com/file"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{
				Header: make(http.Header),
			}
			resp.Header.Set("Location", tt.location)

			RewriteHeaders(resp, domain, proxyBase)

			if resp.Header.Get("Location") != tt.expected {
				t.Errorf("Expected Location %q, got %q", tt.expected, resp.Header.Get("Location"))
			}
		})
	}
}

func TestRewriteHeaders_ContentLocation(t *testing.T) {
	resp := &http.Response{
		Header: make(http.Header),
	}
	resp.Header.Set("Content-Location", "/resource")

	RewriteHeaders(resp, "example.com", "")

	if resp.Header.Get("Content-Location") != "/example.com/resource" {
		t.Errorf("Expected Content-Location to be rewritten, got %q", resp.Header.Get("Content-Location"))
	}
}

func TestRewriteHeaders_Link(t *testing.T) {
	resp := &http.Response{
		Header: make(http.Header),
	}
	resp.Header.Set("Link", `</styles/main.css>; rel="preload", </scripts/app.js>; rel="prefetch"`)

	RewriteHeaders(resp, "example.com", "")

	link := resp.Header.Get("Link")
	if link == "" {
		t.Error("Link header should not be empty")
	}
	// Check that URLs are rewritten
	if !containsAll(link, "/example.com/styles/main.css", "/example.com/scripts/app.js") {
		t.Errorf("Expected Link URLs to be rewritten, got: %s", link)
	}
}

func TestRewriteHeaders_SetCookie(t *testing.T) {
	resp := &http.Response{
		Header: make(http.Header),
	}
	resp.Header.Add("Set-Cookie", "session=abc123; Path=/; HttpOnly")
	resp.Header.Add("Set-Cookie", "user=john; Path=/app")

	RewriteHeaders(resp, "example.com", "")

	cookies := resp.Header.Values("Set-Cookie")
	if len(cookies) != 2 {
		t.Errorf("Expected 2 cookies, got %d", len(cookies))
	}

	// Check that Path is rewritten
	for _, cookie := range cookies {
		if !containsAll(cookie, "/example.com") {
			t.Errorf("Expected cookie Path to be rewritten, got: %s", cookie)
		}
	}
}

func TestRewriteHeaders_RemovesSecurityHeaders(t *testing.T) {
	resp := &http.Response{
		Header: make(http.Header),
	}
	resp.Header.Set("Content-Security-Policy", "default-src 'self'")
	resp.Header.Set("X-Frame-Options", "DENY")

	RewriteHeaders(resp, "example.com", "")

	if resp.Header.Get("Content-Security-Policy") != "" {
		t.Error("CSP header should be removed")
	}
	if resp.Header.Get("X-Frame-Options") != "" {
		t.Error("X-Frame-Options header should be removed")
	}
}

func TestRewriteHeaders_AddsCORS(t *testing.T) {
	resp := &http.Response{
		Header: make(http.Header),
	}

	RewriteHeaders(resp, "example.com", "")

	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Error("CORS header should be added")
	}
}

func TestRewriteRequestHeaders(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://proxy.local/example.com/path", nil)

	RewriteRequestHeaders(req, "example.com", "1.2.3.4", "TestAgent/1.0")

	if req.Host != "example.com" {
		t.Errorf("Expected Host to be example.com, got %s", req.Host)
	}
	if req.Header.Get("X-Forwarded-For") != "1.2.3.4" {
		t.Error("X-Forwarded-For should be set")
	}
	if req.Header.Get("User-Agent") != "TestAgent/1.0" {
		t.Error("User-Agent should be set")
	}
}

func containsAll(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsAt(s, substr))
}

func containsAt(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
