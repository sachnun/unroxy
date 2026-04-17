package rewriter

import (
	"regexp"
	"strings"
)

// CSSRewriter handles CSS content rewriting
type CSSRewriter struct{}

// Regex patterns for CSS
var (
	// Match url() in CSS - handles url(), url("..."), url('...')
	// Go regex doesn't support backreferences, so we match all variants
	cssURLPattern = regexp.MustCompile(`url\(\s*["']?([^"')]+)["']?\s*\)`)

	// Match @import url("...") or @import "..."
	cssImportURLPattern    = regexp.MustCompile(`@import\s+url\(\s*["']?([^"')]+)["']?\s*\)`)
	cssImportStringPattern = regexp.MustCompile(`@import\s+["']([^"']+)["']`)
)

// SupportedContentType returns the content type this rewriter handles
func (r *CSSRewriter) SupportedContentType() string {
	return "text/css"
}

// Rewrite rewrites URLs in CSS content
func (r *CSSRewriter) Rewrite(body []byte, domain, proxyBase string) []byte {
	css := string(body)

	// Rewrite @import url(...) statements
	css = cssImportURLPattern.ReplaceAllStringFunc(css, func(match string) string {
		parts := cssImportURLPattern.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}

		url := strings.TrimSpace(parts[1])
		if url == "" {
			return match
		}

		newURL := ToProxyURL(url, domain, proxyBase)
		return `@import url("` + newURL + `")`
	})

	// Rewrite @import "..." statements
	css = cssImportStringPattern.ReplaceAllStringFunc(css, func(match string) string {
		parts := cssImportStringPattern.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}

		url := strings.TrimSpace(parts[1])
		if url == "" {
			return match
		}

		newURL := ToProxyURL(url, domain, proxyBase)
		return `@import "` + newURL + `"`
	})

	// Rewrite url() in CSS
	css = cssURLPattern.ReplaceAllStringFunc(css, func(match string) string {
		parts := cssURLPattern.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}

		url := strings.TrimSpace(parts[1])

		// Skip data URIs and empty URLs
		if url == "" || strings.HasPrefix(url, "data:") {
			return match
		}

		newURL := ToProxyURL(url, domain, proxyBase)
		return `url("` + newURL + `")`
	})

	return []byte(css)
}
