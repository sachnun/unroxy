package main

import (
	"encoding/base64"
	"net/http"
	"strings"
)

// NamedPool holds a named upstream proxy pool with auth credentials.
type NamedPool struct {
	Name     string
	Username string
	Password string
	Pool     *ProxyPool
	Transport *RotatingProxyTransport
}

// PoolRouter maps auth credentials to proxy pools.
type PoolRouter struct {
	pools            []*NamedPool
	defaultTransport http.RoundTripper
}

// NewPoolRouter creates a pool router. When pools is empty, defaultTransport is used.
func NewPoolRouter(pools []*NamedPool, defaultTransport http.RoundTripper) *PoolRouter {
	if pools == nil {
		pools = []*NamedPool{}
	}
	return &PoolRouter{
		pools:            pools,
		defaultTransport: defaultTransport,
	}
}

// Select finds a transport by username (case-insensitive). Returns nil if not found.
func (r *PoolRouter) Select(username string) *RotatingProxyTransport {
	if username == "" || r == nil {
		return nil
	}

	upper := strings.ToUpper(username)
	for _, p := range r.pools {
		if strings.ToUpper(p.Username) == upper {
			return p.Transport
		}
	}
	return nil
}

// Has checks if a pool name exists (case-insensitive).
func (r *PoolRouter) Has(name string) bool {
	if r == nil || name == "" {
		return false
	}

	upper := strings.ToUpper(name)
	for _, p := range r.pools {
		if strings.ToUpper(p.Name) == upper {
			return true
		}
	}
	return false
}

// Default returns the default transport (all proxies, not a country pool).
func (r *PoolRouter) Default() http.RoundTripper {
	if r == nil {
		return nil
	}

	return r.defaultTransport
}

// Names returns all pool names.
func (r *PoolRouter) Names() []string {
	if r == nil {
		return nil
	}

	names := make([]string, 0, len(r.pools))
	for _, p := range r.pools {
		names = append(names, p.Name)
	}
	return names
}

// PoolCount returns total proxies across all pools (for logging).
func (r *PoolRouter) PoolCount() int {
	if r == nil {
		return 0
	}

	total := 0
	for _, p := range r.pools {
		if p.Pool != nil {
			total += p.Pool.Count()
		}
	}
	return total
}

// AuthUsername extracts the username from a request's Proxy-Authorization or Authorization header.
func AuthUsername(r *http.Request) string {
	user, _, ok := r.BasicAuth()
	if ok {
		return user
	}

	// Manually parse Proxy-Authorization header
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
