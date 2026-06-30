package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type RotatingProxyTransport struct {
	logger         *log.Logger
	pool           *ProxyPool
	transport      http.RoundTripper
	dialTransports sync.Map
	warpTransport  *http.Transport
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

func (t *RotatingProxyTransport) SetWarpTransport(tr *http.Transport) {
	t.warpTransport = tr
}

func (t *RotatingProxyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.warpTransport != nil {
		return t.warpTransport.RoundTrip(req)
	}

	body, hasBody, err := snapshotRequestBody(req)
	if err != nil {
		return nil, err
	}

	targetHost := requestTargetHost(req)
	return t.roundTripViaProxy(req, body, hasBody, targetHost)
}

func (t *RotatingProxyTransport) roundTripViaProxy(req *http.Request, body []byte, hasBody bool, targetHost string) (*http.Response, error) {
	if t.pool == nil {
		return nil, errNoUpstreamProxy
	}

	logger := t.transportLogger()
	targetLog := requestTargetLog(req)

	now := time.Now()
	candidates := t.pool.Candidates(now, targetHost)
	if len(candidates) == 0 {
		return nil, errNoUpstreamProxy
	}

	var lastErr error
	for _, candidate := range candidates {
		attemptReq := cloneRequestForProxy(req, candidate.url, body, hasBody)
		var resp *http.Response
		var err error

		if candidate.dialContext != nil {
			if isPsiphonCandidate(candidate) {
				v, _ := t.dialTransports.LoadOrStore(candidate.key, &http.Transport{
					DialContext:           candidate.dialContext,
					ForceAttemptHTTP2:     false,
					MaxIdleConns:          10,
					IdleConnTimeout:       90 * time.Second,
					TLSHandshakeTimeout:   10 * time.Second,
					ResponseHeaderTimeout: proxyHeaderTimeout,
				})
				resp, err = v.(*http.Transport).RoundTrip(attemptReq)
			} else {
				v, _ := t.dialTransports.LoadOrStore(candidate.key, newUTLSTransport(candidate.dialContext))
				resp, err = v.(*http.Transport).RoundTrip(attemptReq)
			}
		} else {
			resp, err = t.transport.RoundTrip(attemptReq)
		}

		var ti *tunnelInfo
		if isPsiphonCandidate(candidate) {
			ti = TunnelInfoForHost(targetHost)
		}

		proto := candidateProtoPrefix(ti)
		if err != nil {
			if errors.Is(err, errPsiphonNotReady) {
				continue
			}
			if req.Context().Err() != nil {
				lastErr = err
				break
			}
			if isHostUnreachable(err) {
				if !isPsiphonCandidate(candidate) {
					t.pool.MarkFailure(candidate.key, targetHost)
				}
				logger.Printf("[ERR]%s %s -> %s (%v)", proto, targetLog, candidateLogAddress(candidate, ti), err)
				lastErr = err
				break
			}
			if isPsiphonCandidate(candidate) {
				logger.Printf("[ERR]%s %s -> %s (%v)", proto, targetLog, candidateLogAddress(candidate, ti), err)
				lastErr = err
				continue
			}
			t.pool.MarkFailure(candidate.key, targetHost)
			logger.Printf("[ERR]%s %s -> %s (%v)", proto, targetLog, candidateLogAddress(candidate, ti), err)
			lastErr = err
			continue
		}

		if shouldRetryStatus(resp.StatusCode) {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if !isPsiphonCandidate(candidate) {
				t.pool.MarkFailure(candidate.key, targetHost)
			}
			logger.Printf("[RETRY]%s %s -> %s (%d)", proto, targetLog, candidateLogAddress(candidate, ti), resp.StatusCode)
			lastErr = fmt.Errorf("origin returned retriable status %d via %s", resp.StatusCode, candidate.key)
			continue
		}

		t.pool.MarkSuccess(candidate.key, targetHost)
		logger.Printf("[OK]%s %s -> %s (%d)", proto, targetLog, candidateLogAddress(candidate, ti), resp.StatusCode)
		return resp, nil
	}

	if lastErr == nil {
		lastErr = errNoUpstreamProxy
	}

	return nil, lastErr
}

func (t *RotatingProxyTransport) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if t.warpTransport != nil && t.warpTransport.DialContext != nil {
		return t.warpTransport.DialContext(ctx, network, addr)
	}

	targetHost := extractHost(addr)
	logger := t.transportLogger()

	now := time.Now()
	candidates := t.pool.Candidates(now, targetHost)

	if len(candidates) > 0 {
		for _, candidate := range candidates {
			var conn net.Conn
			var err error
			if candidate.dialContext != nil {
				conn, err = candidate.dialContext(ctx, network, addr)
			} else {
				conn, err = httpProxyConnect(ctx, candidate.url, addr)
			}

			var ti *tunnelInfo
			if isPsiphonCandidate(candidate) {
				host, _, _ := net.SplitHostPort(addr)
				ti = TunnelInfoForHost(host)
			}

			proto := candidateProtoPrefix(ti)
			if err != nil {
				if errors.Is(err, errPsiphonNotReady) {
					continue
				}
				if ctx.Err() != nil {
					break
				}
				if isHostUnreachable(err) {
					if !isPsiphonCandidate(candidate) {
						t.pool.MarkFailure(candidate.key, targetHost)
					}
					logger.Printf("[ERR]%s CONNECT %s -> %s (%v)", proto, addr, candidateLogAddress(candidate, ti), err)
					break
				}
				if isPsiphonCandidate(candidate) {
					logger.Printf("[ERR]%s CONNECT %s -> %s (%v)", proto, addr, candidateLogAddress(candidate, ti), err)
					continue
				}
				t.pool.MarkFailure(candidate.key, targetHost)
				logger.Printf("[ERR]%s CONNECT %s -> %s (%v)", proto, addr, candidateLogAddress(candidate, ti), err)
				continue
			}

			t.pool.MarkSuccess(candidate.key, targetHost)
			logger.Printf("[OK]%s CONNECT %s -> %s", proto, addr, candidateLogAddress(candidate, ti))
			return conn, nil
		}
	}

	logger.Printf("[DIRECT] CONNECT %s (no proxy)", addr)
	return (&net.Dialer{Timeout: proxyDialTimeout}).DialContext(ctx, network, addr)
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

func candidateLogAddress(c proxyCandidate, ti *tunnelInfo) string {
	if isPsiphonCandidate(c) && c.psiphon != nil {
		if ti != nil && ti.ip != "" {
			return fmt.Sprintf("%s (%s)", ti.ip, ti.region)
		}
		return "tunnel"
	}

	host := c.url.Hostname()
	if host == "" {
		host = c.url.Host
	}

	if c.country != "" {
		return fmt.Sprintf("%s (%s)", host, c.country)
	}

	return host
}

func candidateProtoPrefix(ti *tunnelInfo) string {
	if ti != nil && ti.protocol != "" {
		return "[TUN]"
	}
	return ""
}

type proxyContextKey struct{}

func newProxyAwareTransport() http.RoundTripper {
	dialer := &net.Dialer{
		Timeout:   proxyDialTimeout,
		KeepAlive: 30 * time.Second,
	}

	return newUTLSTransport(dialer.DialContext)
}

func snapshotRequestBody(req *http.Request) ([]byte, bool, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, false, nil
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, true, err
	}
	if err := req.Body.Close(); err != nil {
		return nil, true, err
	}

	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	req.ContentLength = int64(len(body))

	return body, true, nil
}

func cloneRequestForProxy(req *http.Request, proxyURL *url.URL, body []byte, hasBody bool) *http.Request {
	ctx := req.Context()
	if proxyURL != nil {
		ctx = context.WithValue(ctx, proxyContextKey{}, proxyURL)
	}

	attemptReq := req.Clone(ctx)

	if !hasBody {
		attemptReq.Body = nil
		attemptReq.GetBody = nil
		return attemptReq
	}

	attemptReq.Body = io.NopCloser(bytes.NewReader(body))
	attemptReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	attemptReq.ContentLength = int64(len(body))

	return attemptReq
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

func shouldRetryStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests
}

func isHostUnreachable(err error) bool {
	return strings.Contains(err.Error(), "host unreachable")
}

func isPsiphonCandidate(c proxyCandidate) bool {
	return c.url != nil && c.url.Scheme == "psiphon"
}

func httpProxyConnect(ctx context.Context, proxyURL *url.URL, target string) (net.Conn, error) {
	d := &net.Dialer{Timeout: proxyDialTimeout}
	conn, err := d.DialContext(ctx, "tcp", proxyURL.Host)
	if err != nil {
		return nil, err
	}
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
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
