package rewriter

import (
	"regexp"
)

// JSRewriter handles JavaScript content rewriting
type JSRewriter struct{}

// Regex patterns for JavaScript
// Note: Go regex doesn't support backreferences, so we use separate patterns for each quote type
var (
	// Match static import with double quotes: import x from "/path" or import "/path"
	jsStaticImportDoubleQuote = regexp.MustCompile(`(import\s+(?:[^'"]+\s+from\s+)?)("/[^"]+")`)
	// Match static import with single quotes: import x from '/path' or import '/path'
	jsStaticImportSingleQuote = regexp.MustCompile(`(import\s+(?:[^'"]+\s+from\s+)?)('/[^']+')`)

	// Match dynamic import: import("/path") or import('/path')
	jsDynamicImportDoubleQuote = regexp.MustCompile(`(import\s*\(\s*)("/[^"]+")(\s*\))`)
	jsDynamicImportSingleQuote = regexp.MustCompile(`(import\s*\(\s*)('/[^']+')(\s*\))`)

	// Match fetch("/path") or fetch('/path')
	jsFetchDoubleQuote = regexp.MustCompile(`(fetch\s*\(\s*)("/[^"]+")`)
	jsFetchSingleQuote = regexp.MustCompile(`(fetch\s*\(\s*)('/[^']+')`)

	// Match new URL("/path", ...) or new URL('/path', ...)
	jsURLConstructorDoubleQuote = regexp.MustCompile(`(new\s+URL\s*\(\s*)("/[^"]+")`)
	jsURLConstructorSingleQuote = regexp.MustCompile(`(new\s+URL\s*\(\s*)('/[^']+')`)
)

// SupportedContentType returns the content type this rewriter handles
func (r *JSRewriter) SupportedContentType() string {
	return "application/javascript"
}

// Rewrite rewrites URLs in JavaScript content
func (r *JSRewriter) Rewrite(body []byte, domain, proxyBase string) []byte {
	js := string(body)

	// Helper to rewrite URL in quotes
	rewriteQuotedURL := func(quotedURL string) string {
		quote := quotedURL[0:1]
		url := quotedURL[1 : len(quotedURL)-1]
		newURL := ToProxyURL(url, domain, proxyBase)
		return quote + newURL + quote
	}

	// Rewrite static imports (double quotes)
	js = jsStaticImportDoubleQuote.ReplaceAllStringFunc(js, func(match string) string {
		parts := jsStaticImportDoubleQuote.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		return parts[1] + rewriteQuotedURL(parts[2])
	})

	// Rewrite static imports (single quotes)
	js = jsStaticImportSingleQuote.ReplaceAllStringFunc(js, func(match string) string {
		parts := jsStaticImportSingleQuote.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		return parts[1] + rewriteQuotedURL(parts[2])
	})

	// Rewrite dynamic imports (double quotes)
	js = jsDynamicImportDoubleQuote.ReplaceAllStringFunc(js, func(match string) string {
		parts := jsDynamicImportDoubleQuote.FindStringSubmatch(match)
		if len(parts) != 4 {
			return match
		}
		return parts[1] + rewriteQuotedURL(parts[2]) + parts[3]
	})

	// Rewrite dynamic imports (single quotes)
	js = jsDynamicImportSingleQuote.ReplaceAllStringFunc(js, func(match string) string {
		parts := jsDynamicImportSingleQuote.FindStringSubmatch(match)
		if len(parts) != 4 {
			return match
		}
		return parts[1] + rewriteQuotedURL(parts[2]) + parts[3]
	})

	// Rewrite fetch (double quotes)
	js = jsFetchDoubleQuote.ReplaceAllStringFunc(js, func(match string) string {
		parts := jsFetchDoubleQuote.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		return parts[1] + rewriteQuotedURL(parts[2])
	})

	// Rewrite fetch (single quotes)
	js = jsFetchSingleQuote.ReplaceAllStringFunc(js, func(match string) string {
		parts := jsFetchSingleQuote.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		return parts[1] + rewriteQuotedURL(parts[2])
	})

	// Rewrite new URL (double quotes)
	js = jsURLConstructorDoubleQuote.ReplaceAllStringFunc(js, func(match string) string {
		parts := jsURLConstructorDoubleQuote.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		return parts[1] + rewriteQuotedURL(parts[2])
	})

	// Rewrite new URL (single quotes)
	js = jsURLConstructorSingleQuote.ReplaceAllStringFunc(js, func(match string) string {
		parts := jsURLConstructorSingleQuote.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		return parts[1] + rewriteQuotedURL(parts[2])
	})

	return []byte(js)
}
