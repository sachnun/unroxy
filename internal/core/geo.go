package core

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

// EgressInfo is the externally observed identity of a proxy's exit node.
type EgressInfo struct {
	IP  string
	ISP string
}

// egressTimeout bounds a single egress IP + ISP resolution.
const egressTimeout = 8 * time.Second

var (
	ispCacheMu sync.Mutex
	ispCache   = make(map[string]string) // egress IP -> ISP
)

// dialProxy opens a connection to addr through ps, preferring the proxy's
// own DialContext and falling back to a URL-based HTTP CONNECT.
func dialProxy(ctx context.Context, ps *ProxyState, addr string) (net.Conn, error) {
	if ps.DialContext != nil {
		return ps.DialContext(ctx, "tcp", addr)
	}
	if ps.URL != nil {
		return httpProxyConnect(ctx, ps.URL, addr)
	}
	return nil, errors.New("no proxy dialer")
}

// resolveEgress measures the public egress IP observed through the proxy
// and resolves its ISP. The ISP lookup is a property of the IP and is cached
// globally, so it is fetched at most once per unique IP.
func resolveEgress(ctx context.Context, ps *ProxyState) (EgressInfo, error) {
	dial := ps.DialContext
	if dial == nil {
		if ps.URL == nil {
			return EgressInfo{}, errors.New("no proxy dialer")
		}
		dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return httpProxyConnect(ctx, ps.URL, addr)
		}
	}
	return egressViaDial(ctx, dial)
}

// egressViaDial performs a real HTTPS request through dial and resolves the
// exit IP plus its ISP.
func egressViaDial(ctx context.Context, dial func(ctx context.Context, network, addr string) (net.Conn, error)) (EgressInfo, error) {
	transport := &http.Transport{
		DialContext:         dial,
		ForceAttemptHTTP2:   false,
		MaxIdleConns:        1,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	client := &http.Client{Transport: transport, Timeout: egressTimeout}

	egCtx, cancel := context.WithTimeout(ctx, egressTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(egCtx, http.MethodGet, "https://api.ipify.org?format=json", nil)
	if err != nil {
		return EgressInfo{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return EgressInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return EgressInfo{}, errEgressStatus
	}

	var out struct {
		IP string `json:"ip"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return EgressInfo{}, err
	}
	if out.IP == "" {
		return EgressInfo{}, errors.New("empty egress IP")
	}

	return EgressInfo{IP: out.IP, ISP: ispForIP(egCtx, out.IP)}, nil
}

var errEgressStatus = errors.New("egress endpoint returned non-200")

// ispForIP resolves the ISP for an egress IP using a direct (non-proxied)
// lookup. Results are cached by IP.
func ispForIP(ctx context.Context, ip string) string {
	if ip == "" {
		return ""
	}

	ispCacheMu.Lock()
	if isp, ok := ispCache[ip]; ok {
		ispCacheMu.Unlock()
		return isp
	}
	ispCacheMu.Unlock()

	client := NewProviderHTTPClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://ipwho.is/"+ip, nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var out struct {
		Connection struct {
			ISP string `json:"isp"`
			Org string `json:"org"`
		} `json:"connection"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ""
	}

	isp := out.Connection.ISP
	if isp == "" {
		isp = out.Connection.Org
	}
	if isp == "" {
		return ""
	}

	ispCacheMu.Lock()
	ispCache[ip] = isp
	ispCacheMu.Unlock()
	return isp
}
