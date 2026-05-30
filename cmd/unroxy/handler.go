package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"

	"unroxy/cmd/unroxy/rewriter"
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
	switch {
	case r.Method == http.MethodConnect:
		h.handleConnectTunnel(w, r)
	case r.URL.Host != "":
		h.handleForwardProxy(w, r)
	default:
		h.handleRewriteProxy(w, r)
	}
}

// handleRewriteProxy handles the URL-rewriting proxy mode: /domain/path
func (h *ProxyHandler) handleRewriteProxy(w http.ResponseWriter, r *http.Request) {
	domain, path, query := h.parseRequest(r)
	if domain == "" {
		http.Error(w, "Invalid path. Usage: /domain.com/path", http.StatusBadRequest)
		return
	}

	proxy := h.createProxy(domain, path, query)
	proxy.ServeHTTP(w, r)
}

// handleForwardProxy handles standard HTTP forward proxy (absolute URI)
func (h *ProxyHandler) handleForwardProxy(w http.ResponseWriter, r *http.Request) {
	scheme := r.URL.Scheme
	if scheme != "http" && scheme != "https" {
		http.Error(w, "Unsupported scheme", http.StatusBadRequest)
		return
	}

	domain := r.URL.Host
	path := r.URL.Path
	if path == "" {
		path = "/"
	}

	proxy := h.createForwardProxy(scheme, domain, path, r.URL.RawQuery)
	proxy.ServeHTTP(w, r)
}

// handleConnectTunnel handles CONNECT method (HTTPS tunnel)
func (h *ProxyHandler) handleConnectTunnel(w http.ResponseWriter, r *http.Request) {
	target := r.Host
	if target == "" {
		target = r.URL.Host
	}
	if target == "" {
		http.Error(w, "Missing target host", http.StatusBadRequest)
		return
	}

	// Ensure port is specified
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "443")
	}

	// Get dialer and connect through upstream SOCKS5 pool
	transport, ok := h.transport.(*RotatingProxyTransport)
	if !ok || transport == nil {
		http.Error(w, "Transport not available", http.StatusInternalServerError)
		return
	}

	targetConn, err := transport.DialContext(r.Context(), "tcp", target)
	if err != nil {
		h.logger.Printf("[ERR] CONNECT %s: %v", target, err)
		http.Error(w, "Failed to connect to target", http.StatusServiceUnavailable)
		return
	}
	defer targetConn.Close()

	// Hijack the client connection
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, bufReader, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer clientConn.Close()

	// Send 200 OK to establish tunnel
	if _, err := clientConn.Write([]byte("HTTP/1.1 200 OK\r\n\r\n")); err != nil {
		h.logger.Printf("[ERR] CONNECT %s: failed to send 200: %v", target, err)
		return
	}

	h.logger.Printf("[OK] CONNECT tunnel %s established", target)

	// Bridge connections bidirectionally
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		// Write any buffered data from Hijack to target
		if n := bufReader.Reader.Buffered(); n > 0 {
			io.CopyN(targetConn, bufReader, int64(n))
		}
		io.Copy(targetConn, clientConn)
		targetConn.Close()
	}()

	go func() {
		defer wg.Done()
		io.Copy(clientConn, targetConn)
		clientConn.Close()
	}()

	wg.Wait()
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
	return &httputil.ReverseProxy{
		ErrorLog:  log.New(io.Discard, "", 0),
		Transport: h.transport,
		Director: func(req *http.Request) {
			req.URL.Scheme = "https"
			req.URL.Host = domain
			req.URL.Path = path
			req.URL.RawQuery = query

			rewriter.RewriteRequestHeaders(req, domain)
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Set("Cache-Control", "no-store")
			resp.Header.Set("Pragma", "no-cache")
			resp.Header.Set("Expires", "0")

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

// createForwardProxy creates a reverse proxy for standard HTTP forward proxy mode
func (h *ProxyHandler) createForwardProxy(scheme, domain, path, query string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		ErrorLog:  log.New(io.Discard, "", 0),
		Transport: h.transport,
		Director: func(req *http.Request) {
			req.URL.Scheme = scheme
			req.URL.Host = domain
			req.URL.Path = path
			req.URL.RawQuery = query

			rewriter.RewriteRequestHeaders(req, domain)
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
