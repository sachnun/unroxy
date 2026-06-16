package main

import (
	"encoding/base64"
	"net/http"
	"strings"
)

type NamedPool struct {
	Name     string
	Username string
	Password string
	Pool     *ProxyPool
	Transport *RotatingProxyTransport
}

type PoolRouter struct {
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

func (r *PoolRouter) Default() http.RoundTripper {
	if r == nil {
		return nil
	}

	return r.defaultTransport
}

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

func AuthUsername(r *http.Request) string {
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
