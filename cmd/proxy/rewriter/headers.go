package rewriter

import (
	"net/http"
	"strings"
)

// RewriteHeaders rewrites response headers that contain URLs
func RewriteHeaders(resp *http.Response, domain, proxyBase string) {
	// Rewrite Location header (for redirects)
	if location := resp.Header.Get("Location"); location != "" {
		newLocation := ToProxyURL(location, domain, proxyBase)
		resp.Header.Set("Location", newLocation)
	}

	// Rewrite Content-Location header
	if contentLocation := resp.Header.Get("Content-Location"); contentLocation != "" {
		newContentLocation := ToProxyURL(contentLocation, domain, proxyBase)
		resp.Header.Set("Content-Location", newContentLocation)
	}

	// Rewrite Link header (for preload, prefetch, etc.)
	if link := resp.Header.Get("Link"); link != "" {
		newLink := rewriteLinkHeader(link, domain, proxyBase)
		resp.Header.Set("Link", newLink)
	}

	// Rewrite Set-Cookie Path attribute
	cookies := resp.Header.Values("Set-Cookie")
	if len(cookies) > 0 {
		resp.Header.Del("Set-Cookie")
		for _, cookie := range cookies {
			newCookie := rewriteCookiePath(cookie, domain, proxyBase)
			resp.Header.Add("Set-Cookie", newCookie)
		}
	}

	// Remove headers that might cause issues
	resp.Header.Del("Content-Security-Policy")
	resp.Header.Del("Content-Security-Policy-Report-Only")
	resp.Header.Del("X-Frame-Options")

	// Allow embedding
	resp.Header.Set("Access-Control-Allow-Origin", "*")
}

// rewriteLinkHeader rewrites URLs in Link header
// Format: </path>; rel="preload", <https://example.com/other>; rel="prefetch"
func rewriteLinkHeader(link, domain, proxyBase string) string {
	parts := strings.Split(link, ",")
	var result []string

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		// Extract URL between < and >
		start := strings.Index(part, "<")
		end := strings.Index(part, ">")
		if start == -1 || end == -1 || end <= start {
			result = append(result, part)
			continue
		}

		url := part[start+1 : end]
		newURL := ToProxyURL(url, domain, proxyBase)
		newPart := part[:start+1] + newURL + part[end:]
		result = append(result, newPart)
	}

	return strings.Join(result, ", ")
}

// rewriteCookiePath rewrites the Path attribute in Set-Cookie header
func rewriteCookiePath(cookie, domain, proxyBase string) string {
	// Find Path= in cookie
	lowerCookie := strings.ToLower(cookie)
	pathIdx := strings.Index(lowerCookie, "path=")

	if pathIdx == -1 {
		// No path specified, add one
		return cookie + "; Path=" + proxyBase + "/" + domain + "/"
	}

	// Find the value of Path
	valueStart := pathIdx + 5
	valueEnd := valueStart

	// Find end of path value (next ; or end of string)
	for valueEnd < len(cookie) && cookie[valueEnd] != ';' {
		valueEnd++
	}

	oldPath := strings.TrimSpace(cookie[valueStart:valueEnd])
	newPath := proxyBase + "/" + domain + oldPath

	return cookie[:valueStart] + newPath + cookie[valueEnd:]
}

// RewriteRequestHeaders modifies request headers for proxying
func RewriteRequestHeaders(req *http.Request, domain, randomIP, userAgent string) {
	req.Host = domain

	// Set IP spoofing headers (avoid CF-specific headers)
	req.Header.Set("X-Forwarded-For", randomIP)
	req.Header.Set("X-Real-IP", randomIP)
	req.Header.Set("X-Originating-IP", randomIP)
	req.Header.Set("True-Client-IP", randomIP)
	req.Header.Set("Client-IP", randomIP)

	// Set user agent
	req.Header.Set("User-Agent", userAgent)

	// Disable compression to avoid dealing with gzip/brotli decompression
	req.Header.Set("Accept-Encoding", "identity")

	// Remove headers that might reveal proxy or cause Cloudflare issues
	req.Header.Del("X-Forwarded-Host")
	req.Header.Del("X-Forwarded-Proto")
	req.Header.Del("CF-Connecting-IP") // Cloudflare treats spoofed CF headers as abuse
}
