package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestGenerateRandomIP(t *testing.T) {
	for i := 0; i < 1000; i++ {
		ip := generateRandomIP()

		parsed := net.ParseIP(ip)
		if parsed == nil {
			t.Errorf("invalid IP format: %s", ip)
			continue
		}

		if parsed.IsPrivate() || parsed.IsLoopback() || parsed.IsUnspecified() {
			t.Errorf("generated private/reserved IP: %s", ip)
		}
	}
}

func TestGenerateRandomIPUnique(t *testing.T) {
	ips := make(map[string]bool)
	for i := 0; i < 100; i++ {
		ips[generateRandomIP()] = true
	}
	if len(ips) < 90 {
		t.Errorf("expected at least 90 unique IPs, got %d", len(ips))
	}
}

func TestProxyHeaders(t *testing.T) {
	var capturedHeaders http.Header
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer target.Close()

	targetHost := strings.TrimPrefix(target.URL, "http://")

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		domain := parts[0]
		remainingPath := "/" + strings.Join(parts[1:], "/")

		req, _ := http.NewRequest(r.Method, "http://"+domain+remainingPath, r.Body)

		randomIP := generateRandomIP()
		req.Header.Set("X-Forwarded-For", randomIP)
		req.Header.Set("X-Real-IP", randomIP)
		req.Header.Set("X-Originating-IP", randomIP)
		req.Header.Set("True-Client-IP", randomIP)
		req.Header.Set("Client-IP", randomIP)
		req.Header.Set("User-Agent", userAgents[0])

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer resp.Body.Close()
		io.Copy(w, resp.Body)
	}))
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/" + targetHost + "/test")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	requiredHeaders := []string{
		"X-Forwarded-For",
		"X-Real-Ip",
		"X-Originating-Ip",
		"True-Client-Ip",
		"Client-Ip",
	}

	for _, h := range requiredHeaders {
		val := capturedHeaders.Get(h)
		if val == "" {
			t.Errorf("missing header: %s", h)
			continue
		}
		if net.ParseIP(val) == nil {
			t.Errorf("invalid IP format for %s: %s", h, val)
		}
		parsed := net.ParseIP(val)
		if parsed.IsPrivate() || parsed.IsLoopback() {
			t.Errorf("header %s has private IP: %s", h, val)
		}
	}
}

func TestProxyUserAgent(t *testing.T) {
	var capturedUA string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedUA = r.Header.Get("User-Agent")
		w.Write([]byte("ok"))
	}))
	defer target.Close()

	targetHost := strings.TrimPrefix(target.URL, "http://")

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		domain := parts[0]
		remainingPath := "/" + strings.Join(parts[1:], "/")

		req, _ := http.NewRequest(r.Method, "http://"+domain+remainingPath, nil)
		req.Header.Set("User-Agent", userAgents[0])

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer resp.Body.Close()
		io.Copy(w, resp.Body)
	}))
	defer proxy.Close()

	_, err := http.Get(proxy.URL + "/" + targetHost + "/test")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	if !slices.Contains(userAgents, capturedUA) {
		t.Errorf("unexpected user-agent: %s", capturedUA)
	}
}

func TestURLRewriting(t *testing.T) {
	domain := "example.com"
	cache := getRegexCache(domain)
	defer putRegexCache(cache)

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "absolute URL",
			input:    `<a href="https://example.com/page">`,
			expected: `<a href="/example.com/page">`,
		},
		{
			name:     "relative URL in href",
			input:    `<a href="/page">`,
			expected: `<a href="/example.com/page">`,
		},
		{
			name:     "relative URL in src",
			input:    `<img src="/image.png">`,
			expected: `<img src="/example.com/image.png">`,
		},
		{
			name:     "CSS url",
			input:    `background: url("/style.css")`,
			expected: `background: url("/example.com/style.css")`,
		},
		{
			name:     "skip protocol-relative",
			input:    `<a href="//other.com/page">`,
			expected: `<a href="//other.com/page">`,
		},
		{
			name:     "skip external",
			input:    `<a href="https://other.com/page">`,
			expected: `<a href="https://other.com/page">`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			html := tt.input

			html = cache.absoluteURL.ReplaceAllString(html, "/"+domain+"$1")
			html = cache.attrURL.ReplaceAllStringFunc(html, func(match string) string {
				m := cache.attrURL.FindStringSubmatch(match)
				if len(m) == 3 && strings.HasPrefix(m[2], "/") && !strings.HasPrefix(m[2], "//") && !strings.HasPrefix(m[2], "/"+domain) {
					return m[1] + `="/` + domain + m[2] + `"`
				}
				return match
			})
			html = cache.cssURL.ReplaceAllStringFunc(html, func(match string) string {
				m := cache.cssURL.FindStringSubmatch(match)
				if len(m) == 2 {
					url := strings.Trim(m[1], `'"`)
					if strings.HasPrefix(url, "/") && !strings.HasPrefix(url, "//") && !strings.HasPrefix(url, "/"+domain) {
						return `url("/` + domain + url + `")`
					}
				}
				return match
			})

			if html != tt.expected {
				t.Errorf("\ngot:      %s\nexpected: %s", html, tt.expected)
			}
		})
	}
}
