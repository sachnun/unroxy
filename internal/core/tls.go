package core

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	utls "github.com/refraction-networking/utls"
)

func NewUTLSTransport(dialContext func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Transport {
	return &http.Transport{
		DialTLSContext: uTLSDialer(dialContext),
		Proxy: func(req *http.Request) (*url.URL, error) {
			proxyURL, _ := req.Context().Value(proxyContextKey{}).(*url.URL)
			// https-scheme proxy: the dialContext performs the CONNECT-TLS
			// itself; route directly to avoid double proxying.
			if proxyURL != nil && proxyURL.Scheme == "https" {
				return nil, nil
			}
			return proxyURL, nil
		},
		DialContext:           dialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: HeaderTimeout,
	}
}

func uTLSDialer(dialContext func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		proxyURL, _ := ctx.Value(proxyContextKey{}).(*url.URL)

		var rawConn net.Conn
		var err error

		if _, own := ctx.Value(proxyDialerKey{}).(bool); own {
			// The candidate's DialContext already establishes the connection
			// through the proxy (socks / psiphon / authed CONNECT).
			rawConn, err = dialContext(ctx, network, addr)
		} else if proxyURL != nil {
			rawConn, err = httpProxyConnect(ctx, proxyURL, addr)
		} else {
			rawConn, err = dialContext(ctx, network, addr)
		}
		if err != nil {
			return nil, fmt.Errorf("utls dial: %w", err)
		}

		host, _, serr := net.SplitHostPort(addr)
		if serr != nil {
			rawConn.Close()
			return nil, fmt.Errorf("utls split host: %w", serr)
		}

		uconn := utls.UClient(rawConn, &utls.Config{
			ServerName: host,
		}, utls.HelloChrome_Auto)

		if err := uconn.BuildHandshakeState(); err != nil {
			rawConn.Close()
			return nil, fmt.Errorf("utls build: %w", err)
		}

		for _, ext := range uconn.Extensions {
			if alpn, ok := ext.(*utls.ALPNExtension); ok {
				alpn.AlpnProtocols = []string{"http/1.1"}
				break
			}
		}

		if err := uconn.HandshakeContext(ctx); err != nil {
			rawConn.Close()
			return nil, fmt.Errorf("utls handshake: %w", err)
		}

		return uconn, nil
	}
}
