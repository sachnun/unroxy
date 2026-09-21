package core

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
)

type ProxyHandler struct {
	logger    *log.Logger
	transport http.RoundTripper
	router    *PoolRouter
}

func NewProxyHandler(logger *log.Logger, router *PoolRouter) *ProxyHandler {
	if logger == nil {
		logger = log.Default()
	}

	h := &ProxyHandler{
		logger: logger,
		router: router,
	}
	if router != nil {
		h.transport = router.Default()
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
		h.writeIndexPage(w, r)
	}
}

var errUnknownRegion = errors.New("unknown proxy region")

func (h *ProxyHandler) resolveTransport(r *http.Request) (http.RoundTripper, error) {
	username := authUsername(r)
	if username != "" && h.router != nil {
		if transport := h.router.Select(username); transport != nil {
			return transport, nil
		}
		return nil, errUnknownRegion
	}
	return h.transport, nil
}

func (h *ProxyHandler) writeIndexPage(w http.ResponseWriter, r *http.Request) {
	var buf strings.Builder

	host := r.Host
	if host == "" {
		host = "localhost:8080"
	}
	buf.WriteString("Usage\n")
	buf.WriteString("─────\n")
	fmt.Fprintf(&buf, "  HTTP      curl -x http://%s http://ipwho.is\n", host)
	fmt.Fprintf(&buf, "  CONNECT   curl -x http://%s https://ipwho.is\n", host)
	fmt.Fprintf(&buf, "  Region    curl -x http://us@%s https://ipwho.is\n", host)

	if h.router != nil {
		stats := h.router.Stats()
		if len(stats.Pools) > 0 {
			buf.WriteString("\nPools\n")
			buf.WriteString("─────\n")
			const colWidth = 12
			for i, p := range stats.Pools {
				entry := fmt.Sprintf("%s(%d)", p.Name, p.ProxyCount)
				if i%5 == 0 {
					fmt.Fprintf(&buf, "  %-*s", -colWidth, entry)
				} else {
					fmt.Fprintf(&buf, "%-*s", colWidth, entry)
				}
				if i%5 == 4 || i == len(stats.Pools)-1 {
					buf.WriteString("\n")
				}
			}
			fmt.Fprintf(&buf, "\nTotal: %d proxies\n", stats.TotalProxies)
		}
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(buf.String()))
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

	transport, err := h.resolveTransport(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	h.createProxy(scheme, domain, path, r.URL.RawQuery, transport).ServeHTTP(w, r)
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

	transport, err := h.resolveTransport(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	rt, ok := transport.(*RotatingProxyTransport)
	if !ok || rt == nil {
		http.Error(w, "Transport not available", http.StatusInternalServerError)
		return
	}

	var targetConn net.Conn
	if authUsername(r) != "" {
		targetConn, err = rt.DialContextStrict(r.Context(), "tcp", target)
	} else {
		targetConn, err = rt.DialContext(r.Context(), "tcp", target)
	}
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

func (h *ProxyHandler) createProxy(scheme, domain, path, query string, transport http.RoundTripper) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		ErrorLog:  log.New(io.Discard, "", 0),
		Transport: transport,
		Director: func(req *http.Request) {
			req.URL.Scheme = scheme
			req.URL.Host = domain
			req.URL.Path = path
			req.URL.RawQuery = query
			req.Host = domain
			req.Header["X-Forwarded-For"] = nil
			req.Header.Del("X-Real-IP")
			req.Header.Del("X-Originating-IP")
			req.Header.Del("True-Client-IP")
			req.Header.Del("Client-IP")
			req.Header.Del("Forwarded")
			req.Header.Del("X-Forwarded-Host")
			req.Header.Del("X-Forwarded-Proto")
			req.Header.Del("CF-Connecting-IP")
		},
	}
}
