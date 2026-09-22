package core

import (
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
)

type NamedPool struct {
	Name      string
	Username  string
	Pool      *ProxyPool
	Transport *RotatingProxyTransport
}

type PoolRouter struct {
	mu               sync.RWMutex
	pools            []*NamedPool
	defaultTransport http.RoundTripper
}

func NewPoolRouter(pools []*NamedPool, defaultTransport http.RoundTripper) *PoolRouter {
	if pools == nil {
		pools = []*NamedPool{}
	}
	return &PoolRouter{
		pools:            pools,
		defaultTransport: defaultTransport,
	}
}

func (r *PoolRouter) Add(p *NamedPool) {
	if r == nil || p == nil {
		return
	}
	r.mu.Lock()
	r.pools = append(r.pools, p)
	r.mu.Unlock()
}

func (r *PoolRouter) Select(username string) *RotatingProxyTransport {
	if username == "" || r == nil {
		return nil
	}

	upper := strings.ToUpper(username)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.pools {
		if strings.ToUpper(p.Username) == upper {
			return p.Transport
		}
	}
	return nil
}

func (r *PoolRouter) Default() http.RoundTripper {
	if r == nil {
		return nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defaultTransport
}

func (r *PoolRouter) Names() []string {
	if r == nil {
		return nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.pools))
	for _, p := range r.pools {
		names = append(names, p.Username)
	}
	return names
}

type poolInfo struct {
	Name        string
	ProxyCount  int
	TunnelCount int
	UsableCount int
}

type systemStats struct {
	Pools        []poolInfo
	TotalProxies int
	TotalTunnels int
	TotalUsable  int
}

func (r *PoolRouter) Stats() systemStats {
	if r == nil {
		return systemStats{}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	pools := make([]poolInfo, 0, len(r.pools))
	total := 0
	totalTunnels := 0
	totalUsable := 0
	for _, p := range r.pools {
		count := 0
		tunnels := 0
		usable := 0
		if p.Pool != nil {
			count = p.Pool.Count()
			tunnels = p.Pool.TunnelCount()
			usable = p.Pool.UsableCount()
		}
		pools = append(pools, poolInfo{
			Name: p.Name, ProxyCount: count, TunnelCount: tunnels, UsableCount: usable,
		})
		total += count
		totalTunnels += tunnels
		totalUsable += usable
	}
	return systemStats{
		Pools: pools, TotalProxies: total, TotalTunnels: totalTunnels, TotalUsable: totalUsable,
	}
}

func authUsername(r *http.Request) string {
	user, _, ok := r.BasicAuth()
	if ok {
		return user
	}

	pa := r.Header.Get("Proxy-Authorization")
	if pa == "" {
		return ""
	}

	if !strings.HasPrefix(pa, "Basic ") {
		return ""
	}

	payload, err := base64.StdEncoding.DecodeString(strings.TrimSpace(pa[6:]))
	if err != nil {
		return ""
	}

	pair := strings.SplitN(string(payload), ":", 2)
	if len(pair) == 0 || pair[0] == "" {
		return ""
	}

	return pair[0]
}
