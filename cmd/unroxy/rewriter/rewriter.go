package rewriter

import (
	"net/url"
	"strings"
)

// ToProxyURL converts a URL to a proxied URL
// Examples:
//   - /path/to/file -> /domain/path/to/file
//   - https://domain.com/path -> /domain/path
//   - //cdn.example.com/path -> /cdn.example.com/path
//   - data:..., javascript:..., mailto:..., # -> unchanged
func ToProxyURL(rawURL, domain, proxyBase string) string {
	rawURL = strings.TrimSpace(rawURL)

	// Skip special schemes and empty URLs
	if rawURL == "" ||
		strings.HasPrefix(rawURL, "data:") ||
		strings.HasPrefix(rawURL, "javascript:") ||
		strings.HasPrefix(rawURL, "mailto:") ||
		strings.HasPrefix(rawURL, "tel:") ||
		strings.HasPrefix(rawURL, "#") ||
		strings.HasPrefix(rawURL, "blob:") {
		return rawURL
	}

	// Handle protocol-relative URLs (//cdn.example.com/path)
	if strings.HasPrefix(rawURL, "//") {
		externalDomain := strings.TrimPrefix(rawURL, "//")
		if idx := strings.Index(externalDomain, "/"); idx != -1 {
			return proxyBase + "/" + externalDomain
		}
		return proxyBase + "/" + externalDomain
	}

	// Handle absolute URLs (https://domain.com/path)
	if strings.HasPrefix(rawURL, "http://") || strings.HasPrefix(rawURL, "https://") {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return rawURL
		}
		// Only rewrite URLs from the same domain
		if parsed.Host == domain {
			path := parsed.Path
			if parsed.RawQuery != "" {
				path += "?" + parsed.RawQuery
			}
			if parsed.Fragment != "" {
				path += "#" + parsed.Fragment
			}
			return proxyBase + "/" + domain + path
		}
		// External domain - also proxy it
		path := parsed.Path
		if parsed.RawQuery != "" {
			path += "?" + parsed.RawQuery
		}
		return proxyBase + "/" + parsed.Host + path
	}

	// Handle root-relative URLs (/path/to/file)
	if strings.HasPrefix(rawURL, "/") {
		// Already proxied?
		if strings.HasPrefix(rawURL, proxyBase+"/"+domain) ||
			strings.HasPrefix(rawURL, "/"+domain+"/") {
			return rawURL
		}
		return proxyBase + "/" + domain + rawURL
	}

	// Relative URLs (path/to/file, ../path, ./path) - leave unchanged
	// Browser will resolve these relative to current proxied URL
	return rawURL
}


