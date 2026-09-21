package core

import (
	"errors"
	"net"
	"net/http"
	"time"
)

const (
	DialTimeout          = 5 * time.Second
	HeaderTimeout        = 20 * time.Second
	ProviderFetchTimeout = 30 * time.Second
	FailureTTL           = 10 * time.Minute
)

var errNoUpstreamProxy = errors.New("no upstream proxies available")

func newHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   DialTimeout,
		KeepAlive: 30 * time.Second,
	}
	return &http.Client{
		Timeout:   ProviderFetchTimeout,
		Transport: newUTLSTransport(dialer.DialContext),
	}
}
