package providers

import (
	"log"
	"sync"

	"unroxy/internal/core"
)

type Host struct {
	logger *log.Logger
	router *core.PoolRouter
	pool   *core.ProxyPool

	mu        sync.Mutex
	countries map[string]*core.ProxyPool
	primaries []*core.ProxyState
}

func NewHost(logger *log.Logger) *Host {
	if logger == nil {
		logger = log.Default()
	}
	h := &Host{
		logger:    logger,
		countries: make(map[string]*core.ProxyPool),
	}
	h.pool = core.NewProxyPool(h.logger, nil)
	h.router = core.NewPoolRouter(nil, core.NewRotatingProxyTransport(h.pool))
	return h
}

func (h *Host) Router() *core.PoolRouter { return h.router }

func (h *Host) AddPrimary(ps *core.ProxyState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.primaries = append(h.primaries, ps)
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

func (h *Host) applyPrimariesLocked() {
	for _, ps := range h.primaries {
		h.pool.SetPrimary(ps)
		h.countryLocked(ps.Country).SetPrimary(ps)
	}
}
