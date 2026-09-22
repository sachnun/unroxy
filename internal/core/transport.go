package core

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const notReadyWait = 30 * time.Second

type RotatingProxyTransport struct {
	logger         *log.Logger
	pool           *ProxyPool
	transport      http.RoundTripper
	dialTransports sync.Map
}

func NewRotatingProxyTransport(pool *ProxyPool) *RotatingProxyTransport {
	logger := log.Default()
	if pool != nil && pool.logger != nil {
		logger = pool.logger
	}

	return &RotatingProxyTransport{
		logger:    logger,
		pool:      pool,
		transport: newProxyAwareTransport(),
	}
}

func (t *RotatingProxyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.pool == nil {
		return nil, errNoUpstreamProxy
	}

	targetHost := requestTargetHost(req)
	candidate, ok := t.readyCandidate(req.Context(), targetHost)
	if !ok {
		return nil, errNoUpstreamProxy
	}

	logger := t.transportLogger()
	targetLog := requestTargetLog(req)

	attemptReq := requestWithCandidate(req, candidate)
	var resp *http.Response
	var err error

	if candidate.DialContext != nil {
		if isTunnelCandidate(candidate) {
			v, _ := t.dialTransports.LoadOrStore("tunnel:"+candidate.Key, &http.Transport{
				DialContext:           candidate.DialContext,
				DisableKeepAlives:     true,
				ForceAttemptHTTP2:     false,
				MaxIdleConns:          10,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: HeaderTimeout,
			})
			resp, err = v.(*http.Transport).RoundTrip(attemptReq)
		} else {
			v, _ := t.dialTransports.LoadOrStore(candidate.Key, newUTLSTransport(candidate.DialContext))
			resp, err = v.(*http.Transport).RoundTrip(attemptReq)
		}
	} else {
		resp, err = t.transport.RoundTrip(attemptReq)
	}

	var ti *exitInfo
	if isTunnelCandidate(candidate) {
		ti = exitFor(targetHost)
	}

	proto := candidateProtoPrefix(ti)
	if err != nil {
		if !isTunnelCandidate(candidate) {
			t.pool.MarkFailure(candidate.Key, targetHost)
		}
		logger.Printf("[ERR]%s %s -> %s (%v)", proto, targetLog, candidateLogAddress(candidate, ti), err)
		return nil, err
	}

	t.pool.MarkSuccess(candidate.Key, targetHost)
	logger.Printf("[OK]%s %s -> %s (%d)", proto, targetLog, candidateLogAddress(candidate, ti), resp.StatusCode)
	t.setEgressHeaders(resp, candidate, targetHost)
	return resp, nil
}

func (t *RotatingProxyTransport) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return t.dialContext(ctx, network, addr, false)
}

func (t *RotatingProxyTransport) DialContextStrict(ctx context.Context, network, addr string) (net.Conn, error) {
	return t.dialContext(ctx, network, addr, true)
}

func (t *RotatingProxyTransport) dialContext(ctx context.Context, network, addr string, strict bool) (net.Conn, error) {
	conn, ok := t.dialThroughPool(ctx, network, addr)
	if ok {
		return conn, nil
	}

	if strict {
		return nil, errNoUpstreamProxy
	}
	t.transportLogger().Printf("[DIRECT] CONNECT %s (no proxy)", addr)
	return (&net.Dialer{Timeout: DialTimeout}).DialContext(ctx, network, addr)
}

func (t *RotatingProxyTransport) dialThroughPool(ctx context.Context, network, addr string) (net.Conn, bool) {
	targetHost := extractHost(addr)
	logger := t.transportLogger()

	candidate, ok := t.readyCandidate(ctx, targetHost)
	if !ok {
		return nil, false
	}

	var conn net.Conn
	var err error
	if candidate.DialContext != nil {
		conn, err = candidate.DialContext(ctx, network, addr)
	} else {
		conn, err = httpProxyConnect(ctx, candidate.URL, addr)
	}

	var ti *exitInfo
	if isTunnelCandidate(candidate) {
		host, _, _ := net.SplitHostPort(addr)
		ti = exitFor(host)
	}

	proto := candidateProtoPrefix(ti)
	if err != nil {
		if !isTunnelCandidate(candidate) {
			t.pool.MarkFailure(candidate.Key, targetHost)
		}
		logger.Printf("[ERR]%s CONNECT %s -> %s (%v)", proto, addr, candidateLogAddress(candidate, ti), err)
		return nil, false
	}

	t.pool.MarkSuccess(candidate.Key, targetHost)
	logger.Printf("[OK]%s CONNECT %s -> %s", proto, addr, candidateLogAddress(candidate, ti))
	return conn, true
}

func (t *RotatingProxyTransport) readyCandidate(ctx context.Context, targetHost string) (ProxyCandidate, bool) {
	deadline := time.Now().Add(notReadyWait)
	for {
		candidates := t.pool.Candidates(time.Now(), targetHost)
		if len(candidates) == 0 {
			return ProxyCandidate{}, false
		}
		if hasReadyTunnel(candidates) || !time.Now().Before(deadline) {
			return candidates[0], true
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return candidates[0], true
		}
	}
}

func hasReadyTunnel(candidates []ProxyCandidate) bool {
	for _, candidate := range candidates {
		if candidate.Tunnel == nil || candidate.Tunnel.IsReady() {
			return true
		}
	}
	return false
}

func (t *RotatingProxyTransport) transportLogger() *log.Logger {
	logger := t.logger
	if logger == nil {
		logger = log.Default()
		if t.pool != nil && t.pool.logger != nil {
			logger = t.pool.logger
		}
	}

	return logger
}

func candidateLogAddress(c ProxyCandidate, ti *exitInfo) string {
	if isTunnelCandidate(c) && c.Tunnel != nil {
		if ti != nil && ti.ip != "" {
			return fmt.Sprintf("%s (%s)", ti.ip, ti.region)
		}
		return "tunnel"
	}

	host := c.URL.Hostname()
	if host == "" {
		host = c.URL.Host
	}

	if c.Country != "" {
		return fmt.Sprintf("%s (%s)", host, c.Country)
	}

	return host
}

func candidateProtoPrefix(ti *exitInfo) string {
	if ti != nil && ti.protocol != "" {
		return "[TUN]"
	}
	return ""
}

func (t *RotatingProxyTransport) setEgressHeaders(resp *http.Response, c ProxyCandidate, targetHost string) {
	if resp == nil {
		return
	}
	if resp.Header == nil {
		resp.Header = make(http.Header)
	}

	ip, isp := c.IP, c.ISP
	if isTunnelCandidate(c) {
		if ti := exitFor(targetHost); ti != nil && ti.ip != "" {
			ip = ti.ip
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			isp = ispForIP(ctx, ti.ip)
			cancel()
		}
	}

	if ip != "" {
		resp.Header.Set("x-unroxy-ip", ip)
	}
	if isp != "" {
		resp.Header.Set("x-unroxy-isp", isp)
	}
}

type proxyContextKey struct{}

type proxyDialerKey struct{}

func newProxyAwareTransport() http.RoundTripper {
	dialer := &net.Dialer{
		Timeout:   DialTimeout,
		KeepAlive: 30 * time.Second,
	}

	return newUTLSTransport(dialer.DialContext)
}

func requestWithCandidate(req *http.Request, candidate ProxyCandidate) *http.Request {
	ctx := req.Context()
	if candidate.URL != nil {
		ctx = context.WithValue(ctx, proxyContextKey{}, candidate.URL)
	}
	if candidate.DialContext != nil {
		ctx = context.WithValue(ctx, proxyDialerKey{}, true)
	}
	return req.Clone(ctx)
}

func requestTargetHost(req *http.Request) string {
	if req == nil || req.URL == nil {
		return ""
	}

	host := strings.ToLower(req.URL.Hostname())
	if host != "" {
		return host
	}

	return strings.ToLower(req.URL.Host)
}

func requestTargetLog(req *http.Request) string {
	if req == nil || req.URL == nil {
		return "-"
	}

	host := req.URL.Host
	if hostname := req.URL.Hostname(); hostname != "" {
		host = hostname
	}
	if host == "" {
		host = "-"
	}

	path := req.URL.EscapedPath()
	if path == "" || path == "/" {
		path = ""
	}
	if req.URL.RawQuery != "" {
		path += "?" + req.URL.RawQuery
	}

	return strings.ToLower(host) + path
}

func extractHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return strings.ToLower(host)
}

func isHostUnreachable(err error) bool {
	return strings.Contains(err.Error(), "host unreachable")
}

func isTunnelCandidate(c ProxyCandidate) bool {
	return c.URL != nil && c.URL.Scheme == "psiphon"
}

func httpProxyConnect(ctx context.Context, proxyURL *url.URL, target string) (net.Conn, error) {
	d := &net.Dialer{Timeout: DialTimeout}
	conn, err := d.DialContext(ctx, "tcp", proxyURL.Host)
	if err != nil {
		return nil, err
	}

	if proxyURL.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName:         proxyURL.Hostname(),
			InsecureSkipVerify: true,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("proxy tls handshake: %w", err)
		}
		conn = tlsConn
	}

	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if proxyURL.User != nil {
		user := proxyURL.User.Username()
		pass, _ := proxyURL.User.Password()
		req += fmt.Sprintf("Proxy-Authorization: Basic %s\r\n",
			base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	}
	req += "\r\n"

	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, err
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		conn.Close()
		return nil, err
	}
	resp := buf[:n]
	if !bytes.Contains(resp, []byte("200")) {
		conn.Close()
		firstLine, _, _ := strings.Cut(string(resp), "\r\n")
		return nil, fmt.Errorf("proxy rejected CONNECT: %s", firstLine)
	}
	conn.SetDeadline(time.Time{})
	return conn, nil
}
