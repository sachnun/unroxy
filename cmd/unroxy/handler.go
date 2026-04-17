package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"

	"unroxy/cmd/unroxy/rewriter"
	"unroxy/cmd/unroxy/utils"
)

// ProxyHandler handles all proxy requests
type ProxyHandler struct {
	htmlRewriter *rewriter.HTMLRewriter
	cssRewriter  *rewriter.CSSRewriter
	jsRewriter   *rewriter.JSRewriter
	logger       *log.Logger
	transport    http.RoundTripper
}

// NewProxyHandler creates a new proxy handler
func NewProxyHandler() *ProxyHandler {
	return NewProxyHandlerWithLoggerAndTransport(log.Default(), nil)
}

// NewProxyHandlerWithTransport creates a new proxy handler with a custom transport.
func NewProxyHandlerWithTransport(transport http.RoundTripper) *ProxyHandler {
	return NewProxyHandlerWithLoggerAndTransport(log.Default(), transport)
}

// NewProxyHandlerWithLoggerAndTransport creates a new proxy handler with a logger and custom transport.
func NewProxyHandlerWithLoggerAndTransport(logger *log.Logger, transport http.RoundTripper) *ProxyHandler {
	if logger == nil {
		logger = log.Default()
	}

	return &ProxyHandler{
		htmlRewriter: &rewriter.HTMLRewriter{},
		cssRewriter:  &rewriter.CSSRewriter{},
		jsRewriter:   &rewriter.JSRewriter{},
		logger:       logger,
		transport:    transport,
	}
}

// ServeHTTP handles incoming requests
func (h *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	domain, path, query := h.parseRequest(r)
	if domain == "" {
		h.logger.Printf("request method=%s source=%s invalid_path=true remote=%s", r.Method, requestSource(r), r.RemoteAddr)
		http.Error(w, "Invalid path. Usage: /domain.com/path", http.StatusBadRequest)
		return
	}

	h.logger.Printf("request method=%s source=%s target=%s remote=%s", r.Method, requestSource(r), targetURL(domain, path, query), r.RemoteAddr)

	proxy := h.createProxy(domain, path, query)
	proxy.ServeHTTP(w, r)
}

// parseRequest extracts domain, path, and query from request
func (h *ProxyHandler) parseRequest(r *http.Request) (domain, path, query string) {
	fullPath := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(fullPath, "/", 2)

	if len(parts) < 1 || parts[0] == "" {
		return "", "", ""
	}

	domain = parts[0]
	path = "/"
	if len(parts) > 1 {
		path = "/" + parts[1]
	}

	// Preserve query string
	query = r.URL.RawQuery

	return domain, path, query
}

// createProxy creates a configured reverse proxy
func (h *ProxyHandler) createProxy(domain, path, query string) *httputil.ReverseProxy {
	randomIP := utils.GenerateRandomIP()
	userAgent := utils.RandomUserAgent()

	return &httputil.ReverseProxy{
		Transport: h.transport,
		Director: func(req *http.Request) {
			req.URL.Scheme = "https"
			req.URL.Host = domain
			req.URL.Path = path
			req.URL.RawQuery = query

			rewriter.RewriteRequestHeaders(req, domain, randomIP, userAgent)
		},
		ModifyResponse: func(resp *http.Response) error {
			// Rewrite response headers (Location, Set-Cookie, etc.)
			rewriter.RewriteHeaders(resp, domain, "")

			// Check content type for body rewriting
			contentType := resp.Header.Get("Content-Type")

			// Determine if we need to rewrite body
			needsRewrite := strings.Contains(contentType, "text/html") ||
				strings.Contains(contentType, "text/css") ||
				strings.Contains(contentType, "javascript")

			if !needsRewrite {
				return nil
			}

			// Read response body
			body, err := h.readResponseBody(resp)
			if err != nil {
				return err
			}

			// Rewrite body based on content type
			var newBody []byte
			switch {
			case strings.Contains(contentType, "text/html"):
				newBody = h.htmlRewriter.Rewrite(body, domain, "")
			case strings.Contains(contentType, "text/css"):
				newBody = h.cssRewriter.Rewrite(body, domain, "")
			case strings.Contains(contentType, "javascript"):
				newBody = h.jsRewriter.Rewrite(body, domain, "")
			default:
				newBody = body
			}

			// Set new body
			resp.Body = io.NopCloser(bytes.NewReader(newBody))
			resp.ContentLength = int64(len(newBody))
			resp.Header.Set("Content-Length", strconv.Itoa(len(newBody)))

			// Remove Content-Encoding since we've decompressed
			resp.Header.Del("Content-Encoding")

			return nil
		},
	}
}

// readResponseBody reads and potentially decompresses response body
func (h *ProxyHandler) readResponseBody(resp *http.Response) ([]byte, error) {
	var reader io.Reader = resp.Body

	// Handle gzip encoding
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gzipReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer gzipReader.Close()
		reader = gzipReader
	}

	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()

	return body, nil
}

func requestSource(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}

	if r.URL.RawQuery == "" {
		return r.URL.Path
	}

	return r.URL.Path + "?" + r.URL.RawQuery
}

func targetURL(domain, path, query string) string {
	target := "https://" + domain + path
	if query == "" {
		return target
	}

	return target + "?" + query
}
