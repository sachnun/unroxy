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

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
	"unroxy/cmd/unroxy/rewriter"
)

type ProxyHandler struct {
	htmlRewriter *rewriter.HTMLRewriter
	cssRewriter  *rewriter.CSSRewriter
	jsRewriter   *rewriter.JSRewriter
	logger       *log.Logger
	transport    http.RoundTripper
	router       *PoolRouter
}

func NewProxyHandler(logger *log.Logger, transportOrRouter interface{}) *ProxyHandler {
	if logger == nil {
		logger = log.Default()
	}

	h := &ProxyHandler{
		htmlRewriter: &rewriter.HTMLRewriter{},
		cssRewriter:  &rewriter.CSSRewriter{},
		jsRewriter:   &rewriter.JSRewriter{},
		logger:       logger,
	}

	switch v := transportOrRouter.(type) {
	case *PoolRouter:
		h.router = v
		h.transport = v.Default()
	case http.RoundTripper:
		h.transport = v
	}

	return h
}

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

func (h *ProxyHandler) resolveTransport(r *http.Request) http.RoundTripper {
	username := AuthUsername(r)
	if username != "" && h.router != nil {
		if transport := h.router.Select(username); transport != nil {
			return transport
		}
	}
	return h.transport
}

func (h *ProxyHandler) handleRewriteProxy(w http.ResponseWriter, r *http.Request) {
	poolName, domain, path, query := h.parsePoolRequest(r)
	if domain == "" {
		http.Error(w, "Invalid path. Usage: /domain.com/path", http.StatusBadRequest)
		return
	}

	transport := h.transport
	if poolName != "" && h.router != nil {
		if t := h.router.Select(poolName); t != nil {
			transport = t
		}
	}

	proxy := h.createProxy(domain, path, query, transport)
	proxy.ServeHTTP(w, r)
}

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

	transport := h.resolveTransport(r)
	proxy := h.createForwardProxy(scheme, domain, path, r.URL.RawQuery, transport)
	proxy.ServeHTTP(w, r)
}

func (h *ProxyHandler) handleConnectTunnel(w http.ResponseWriter, r *http.Request) {
	target := r.Host
	if target == "" {
		target = r.URL.Host
	}
	if target == "" {
		http.Error(w, "Missing target host", http.StatusBadRequest)
		return
	}

	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "443")
	}

	transport := h.resolveTransport(r)
	rt, ok := transport.(*RotatingProxyTransport)
	if !ok || rt == nil {
		http.Error(w, "Transport not available", http.StatusInternalServerError)
		return
	}

	targetConn, err := rt.DialContext(r.Context(), "tcp", target)
	if err != nil {
		h.logger.Printf("[ERR] CONNECT %s: %v", target, err)
		http.Error(w, "Failed to connect to target", http.StatusServiceUnavailable)
		return
	}
	defer targetConn.Close()

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

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 OK\r\n\r\n")); err != nil {
		h.logger.Printf("[ERR] CONNECT %s: failed to send 200: %v", target, err)
		return
	}

	h.logger.Printf("[OK] CONNECT tunnel %s established", target)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
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

func (h *ProxyHandler) parsePoolRequest(r *http.Request) (pool, domain, path, query string) {
	fullPath := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(fullPath, "/", 3)

	if len(parts) < 1 || parts[0] == "" {
		return "", "", "", ""
	}

	first := parts[0]

	if h.router != nil && !strings.Contains(first, ".") && h.router.Has(strings.ToUpper(first)) {
		pool = strings.ToUpper(first)
		if len(parts) > 1 {
			domain = parts[1]
			path = "/"
			if len(parts) > 2 {
				path = "/" + strings.Join(parts[2:], "/")
			}
		}
	} else {
		domain = first
		path = "/"
		if len(parts) > 1 {
			path = "/" + strings.Join(parts[1:], "/")
		}
	}

	if domain != "" && !isValidDomain(domain) {
		return "", "", "", ""
	}

	query = r.URL.RawQuery
	return pool, domain, path, query
}

func isValidDomain(s string) bool {
	ascii, err := idna.ToASCII(s)
	if err != nil || ascii == "" {
		return false
	}
	parts := strings.Split(ascii, ".")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if len(part) == 0 {
			return false
		}
	}
	_, isICANN := publicsuffix.PublicSuffix(ascii)
	return isICANN
}

func (h *ProxyHandler) createProxy(domain, path, query string, transport http.RoundTripper) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		ErrorLog:  log.New(io.Discard, "", 0),
		Transport: transport,
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

			rewriter.RewriteHeaders(resp, domain, "")

			contentType := resp.Header.Get("Content-Type")
			needsRewrite := strings.Contains(contentType, "text/html") ||
				strings.Contains(contentType, "text/css") ||
				strings.Contains(contentType, "javascript")

			if !needsRewrite {
				return nil
			}

			body, err := h.readResponseBody(resp)
			if err != nil {
				return err
			}

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

			resp.Body = io.NopCloser(bytes.NewReader(newBody))
			resp.ContentLength = int64(len(newBody))
			resp.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
			resp.Header.Del("Content-Encoding")

			return nil
		},
	}
}

func (h *ProxyHandler) createForwardProxy(scheme, domain, path, query string, transport http.RoundTripper) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		ErrorLog:  log.New(io.Discard, "", 0),
		Transport: transport,
		Director: func(req *http.Request) {
			req.URL.Scheme = scheme
			req.URL.Host = domain
			req.URL.Path = path
			req.URL.RawQuery = query

			rewriter.RewriteRequestHeaders(req, domain)
		},
	}
}

func (h *ProxyHandler) readResponseBody(resp *http.Response) ([]byte, error) {
	var reader io.Reader = resp.Body

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
