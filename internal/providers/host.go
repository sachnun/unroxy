package providers

import (
	"log"
	"sort"
	"strings"
	"sync"

	"unroxy/internal/core"
)

// Host is what a Provider receives to contribute capacity. It owns the
// router, the default proxy pool and lazily-created country pools, and it
// reapplies psiphon primaries whenever a pool's contents are replaced.
type Host struct {
	logger *log.Logger
	router *core.PoolRouter
	pool   *core.ProxyPool

	mu        sync.Mutex
	countries map[string]*core.ProxyPool
	primaries []*core.ProxyState
}

// NewHost builds a router with an empty default pool and no countries.
func NewHost(logger *log.Logger) *Host {
	if logger == nil {
		logger = log.Default()
	}
	pool := core.NewProxyPool(logger, nil)
	router := core.NewPoolRouter(nil, core.NewRotatingProxyTransport(pool))
	return &Host{
		logger:    logger,
		pool:      pool,
		router:    router,
		countries: make(map[string]*core.ProxyPool),
	}
}

// Router exposes the pool router (for named pools, e.g. WARP variants).
func (h *Host) Router() *core.PoolRouter { return h.router }

// DefaultPool returns the pool used when no pool is selected by auth.
func (h *Host) DefaultPool() *core.ProxyPool { return h.pool }

// Country returns the pool for a country code, creating and registering it
// on the router on first use.
func (h *Host) Country(code string) *core.ProxyPool {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		code = "XX"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if pool, ok := h.countries[code]; ok {
		return pool
	}
	pool := core.NewProxyPool(h.logger, nil)
	transport := core.NewRotatingProxyTransport(pool)
	h.countries[code] = pool
	h.router.Add(&core.NamedPool{
		Name:      code,
		Username:  code,
		Pool:      pool,
		Transport: transport,
	})
	h.applyPrimariesLocked()
	return pool
}

// AddNamed registers a named pool with a custom transport (e.g. WARP).
func (h *Host) AddNamed(name string, pool *core.ProxyPool, transport *core.RotatingProxyTransport) {
	h.router.Add(&core.NamedPool{
		Name:      name,
		Username:  name,
		Pool:      pool,
		Transport: transport,
	})
}

// AddPrimary records a tunnel-based primary (psiphon) and applies it to the
// default pool and any matching country pool.
func (h *Host) AddPrimary(ps *core.ProxyState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.primaries = append(h.primaries, ps)
	h.applyPrimariesLocked()
}

// ReplaceProxies replaces the default pool and all country pools with a
// freshly fetched list, then reapplies registered primaries.
func (h *Host) ReplaceProxies(proxies []*core.ProxyState) {
	h.mu.Lock()
	defer h.mu.Unlock()

	groups := core.GroupProxiesByCountry(proxies)
	codes := make([]string, 0, len(groups))
	for code := range groups {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	for _, code := range codes {
		pool := h.countryLocked(code)
		pool.Replace(groups[code])
	}
	h.pool.Replace(proxies)
	h.applyPrimariesLocked()
}

func (h *Host) countryLocked(code string) *core.ProxyPool {
	if pool, ok := h.countries[code]; ok {
		return pool
	}
	pool := core.NewProxyPool(h.logger, nil)
	h.countries[code] = pool
	h.router.Add(&core.NamedPool{
		Name:      code,
		Username:  code,
		Pool:      pool,
		Transport: core.NewRotatingProxyTransport(pool),
	})
	return pool
}

// applyPrimariesLocked re-adds psiphon primaries to the default pool and to
// country pools whose code matches the primary's region exactly (case
// preserved, matching the historical behavior).
func (h *Host) applyPrimariesLocked() {
	for _, ps := range h.primaries {
		h.pool.SetPrimary(ps)
		if pool, ok := h.countries[ps.Country]; ok {
			pool.SetPrimary(ps)
		}
	}
}
