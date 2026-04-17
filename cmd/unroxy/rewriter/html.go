package rewriter

import (
	"regexp"
	"strings"
)

// HTMLRewriter handles HTML content rewriting
type HTMLRewriter struct{}

// Regex patterns for HTML attributes
var (
	// Match src, href, action, data-src, data-href, poster attributes
	attrPattern = regexp.MustCompile(`(?i)(src|href|action|data-src|data-href|poster)=["']([^"']*?)["']`)

	// Match srcset attribute (special format: url size, url size, ...)
	srcsetPattern = regexp.MustCompile(`(?i)srcset=["']([^"']*?)["']`)

	// Match content attribute in meta refresh
	metaRefreshPattern = regexp.MustCompile(`(?i)<meta[^>]+http-equiv=["']refresh["'][^>]+content=["']([^"']*?)["']`)

	// Match inline style with url()
	inlineStylePattern = regexp.MustCompile(`(?i)style=["']([^"']*?)["']`)

	// Match <base href="...">
	baseHrefPattern = regexp.MustCompile(`(?i)<base[^>]+href=["']([^"']*?)["']`)
)

// SupportedContentType returns the content type this rewriter handles
func (r *HTMLRewriter) SupportedContentType() string {
	return "text/html"
}

// Rewrite rewrites URLs in HTML content
func (r *HTMLRewriter) Rewrite(body []byte, domain, proxyBase string) []byte {
	html := string(body)

	// Remove or rewrite <base> tag to prevent it from affecting relative URLs
	html = baseHrefPattern.ReplaceAllString(html, `<base href="`+proxyBase+"/"+domain+`/"`)

	// Rewrite standard attributes (src, href, action, etc.)
	html = attrPattern.ReplaceAllStringFunc(html, func(match string) string {
		parts := attrPattern.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		attr := parts[1]
		url := parts[2]

		// Skip empty URLs
		if url == "" {
			return match
		}

		newURL := ToProxyURL(url, domain, proxyBase)
		return attr + `="` + newURL + `"`
	})

	// Rewrite srcset attribute
	html = srcsetPattern.ReplaceAllStringFunc(html, func(match string) string {
		parts := srcsetPattern.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}
		srcset := parts[1]
		newSrcset := rewriteSrcset(srcset, domain, proxyBase)
		return `srcset="` + newSrcset + `"`
	})

	// Rewrite meta refresh URLs
	html = metaRefreshPattern.ReplaceAllStringFunc(html, func(match string) string {
		parts := metaRefreshPattern.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}
		content := parts[1]
		newContent := rewriteMetaRefresh(content, domain, proxyBase)
		return strings.Replace(match, parts[1], newContent, 1)
	})

	// Rewrite inline styles with url()
	html = inlineStylePattern.ReplaceAllStringFunc(html, func(match string) string {
		parts := inlineStylePattern.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}
		style := parts[1]
		if !strings.Contains(style, "url(") {
			return match
		}
		newStyle := rewriteCSSURLs(style, domain, proxyBase)
		return `style="` + newStyle + `"`
	})

	return []byte(html)
}

// rewriteSrcset rewrites URLs in srcset attribute
// Format: "url1 1x, url2 2x" or "url1 100w, url2 200w"
func rewriteSrcset(srcset, domain, proxyBase string) string {
	entries := strings.Split(srcset, ",")
	var result []string

	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		// Split into URL and size descriptor
		parts := strings.Fields(entry)
		if len(parts) == 0 {
			continue
		}

		url := parts[0]
		newURL := ToProxyURL(url, domain, proxyBase)

		if len(parts) > 1 {
			result = append(result, newURL+" "+strings.Join(parts[1:], " "))
		} else {
			result = append(result, newURL)
		}
	}

	return strings.Join(result, ", ")
}

// rewriteMetaRefresh rewrites URL in meta refresh content
// Format: "5; url=/path" or "0;url=https://example.com"
func rewriteMetaRefresh(content, domain, proxyBase string) string {
	// Find URL part
	lowerContent := strings.ToLower(content)
	urlIdx := strings.Index(lowerContent, "url=")
	if urlIdx == -1 {
		return content
	}

	prefix := content[:urlIdx+4]
	urlPart := strings.TrimSpace(content[urlIdx+4:])

	// Remove surrounding quotes if present
	urlPart = strings.Trim(urlPart, `"'`)

	newURL := ToProxyURL(urlPart, domain, proxyBase)
	return prefix + newURL
}

// rewriteCSSURLs rewrites url() in CSS (used for inline styles)
func rewriteCSSURLs(css, domain, proxyBase string) string {
	cssRewriter := &CSSRewriter{}
	return string(cssRewriter.Rewrite([]byte(css), domain, proxyBase))
}
