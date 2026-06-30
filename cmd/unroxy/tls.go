package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	utls "github.com/refraction-networking/utls"
)

func newUTLSTransport(dialContext func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Transport {
	return &http.Transport{
		DialTLSContext: uTLSDialer(dialContext),
		Proxy: func(req *http.Request) (*url.URL, error) {
			proxyURL, _ := req.Context().Value(proxyContextKey{}).(*url.URL)
			return proxyURL, nil
		},
		DialContext:           dialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: proxyHeaderTimeout,
	}
}

func newProviderHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   proxyDialTimeout,
		KeepAlive: 30 * time.Second,
	}
	return &http.Client{
		Timeout:   providerFetchTimeout,
		Transport: newUTLSTransport(dialer.DialContext),
	}
}

func uTLSDialer(dialContext func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		proxyURL, _ := ctx.Value(proxyContextKey{}).(*url.URL)

		var rawConn net.Conn
		var err error

		if proxyURL != nil {
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
